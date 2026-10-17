package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/filter"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	procspkg "github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
	"github.com/s1crab/android-log-mcp/server/internal/timeparse"
)

const (
	scanBudget    = 2500 * time.Millisecond
	maxWaitMs     = 25000
	defaultLimit  = 50 // #23: was 200
	contextScanMx = 20000
)

type query struct {
	d          *device.Device
	loc        *time.Location
	pred       filter.Node
	parts      []string
	notes      []string
	fromNs     int64
	toNs       int64
	afterSeq   uint64
	beforeSeq  uint64
	ectx       *filter.Ctx
	pkg        string
	tags       []string
	filterUsed bool
	verbose    bool
	clock      device.ClockInfo // device now used for relative values
	relUpper   bool             // toNs is device now + relSlack (relative window)
}

func (q *query) interp() string {
	if len(q.parts) == 0 {
		return "(none)"
	}
	if len(q.parts) == 1 {
		return q.parts[0]
	}
	// make the real precedence explicit: every part is ANDed with the others
	out := make([]string, len(q.parts))
	for i, p := range q.parts {
		if topLevelOr(p) {
			p = "(" + p + ")"
		}
		out[i] = p
	}
	return strings.Join(out, " AND ")
}

// topLevelOr reports whether s has an OR outside parentheses/quotes.
func topLevelOr(s string) bool {
	depth, inQ, inRe := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQ:
			if c == '\\' {
				i++
			} else if c == '"' {
				inQ = false
			}
		case inRe:
			if c == '\\' {
				i++
			} else if c == '/' {
				inRe = false
			}
		case c == '"':
			inQ = true
		case c == '/' && i > 0 && s[i-1] == ' ':
			inRe = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], " OR "):
			return true
		}
	}
	return false
}

func (q *query) inTime(r *model.Record) bool {
	return (q.fromNs == 0 || r.TimeNs >= q.fromNs) && (q.toNs == 0 || r.TimeNs < q.toNs)
}

func pkgMatches(r *model.Record, pkg string) bool {
	return r.Package == pkg || r.Process == pkg || strings.HasPrefix(r.Process, pkg+":")
}

// topLevelAge returns the narrowest age: term that every match must satisfy
// (the filter itself, or a term of its top-level AND), with its source text.
func topLevelAge(n filter.Node) (time.Duration, string, bool) {
	switch x := n.(type) {
	case *filter.Age:
		return x.Dur, x.Src, true
	case *filter.And:
		var best time.Duration
		src, ok := "", false
		for _, k := range x.Kids {
			if d, s, kok := topLevelAge(k); kok && (!ok || d < best) {
				best, src, ok = d, s, true
			}
		}
		return best, src, ok
	}
	return 0, "", false
}

type commonArgs struct {
	verbose                                        bool
	filter, pkg, minLevel, from, to, before, after string
	tags                                           []string
	wait                                           bool // waiting for new lines (wait_ms): no upper bound
}

var reAgeHost = regexp.MustCompile(`(?i)(^|[\s(!-])age:\s*host:(\S*)`)

// relSlack: a relative window ends at device now + relSlack (issue #48),
// so lines dated after a clock set back are not in "the last 5 minutes".
const relSlack = 2 * time.Second

// hostRejected is the error for a host: value (issue #48 D2): queries use
// device time only.
func hostRejected(name, v string, ci device.ClockInfo) error {
	if name == "age" {
		return errc("host_time", []string{`filter="age:5m"`}, "param", "age", "value", "host:"+v, "dev_now", isoSec(ci.Now))
	}
	return errc("host_time", []string{name + `="30m"`, "logcat_status"}, "param", name, "dev_now", isoSec(ci.Now))
}

// badTime maps a from/to parse failure to the coded error.
func badTime(name, v string) error {
	ex := `from="30m"`
	if name == "to" {
		ex = `to="now"`
	}
	return errc("bad_time", []string{ex}, "param", name, "value", v, "valid", "30m|2h|HH:MM:SS|YYYY-MM-DD[ HH:MM:SS]|ISO-8601")
}

func (t *Tools) buildQuery(d *device.Device, a commonArgs) (*query, error) {
	if timeparse.HostPrefixed(a.from) {
		return nil, hostRejected("from", a.from, d.Clock())
	}
	if timeparse.HostPrefixed(a.to) {
		return nil, hostRejected("to", a.to, d.Clock())
	}
	if m := reAgeHost.FindStringSubmatch(a.filter); m != nil {
		return nil, hostRejected("age", m[2], d.Clock())
	}
	// device now comes from the stream: no device clock read per query
	ci := d.Clock()
	now := ci.Now
	q := &query{d: d, loc: d.Loc(), ectx: &filter.Ctx{Now: now, Loc: d.Loc()}, verbose: a.verbose, pkg: a.pkg, tags: a.tags, clock: ci}
	var kids []filter.Node
	var root filter.Node // the parsed filter
	if a.pkg != "" {
		pkg := strings.TrimSpace(a.pkg)
		q.pkg = pkg
		su := newSharedUID(pkg, d.Packages(), d.Procs)
		kids = append(kids, &filter.Pred{Desc: "package == " + pkg, ClassOnly: true, Fn: func(r *model.Record, _ *filter.Ctx) bool { return pkgMatches(r, pkg) || su.line(r.UID, r.Process) }})
		if su != nil {
			q.notes = append(q.notes, su.noteLine(a.verbose))
		}
		q.parts = append(q.parts, "package == "+pkg)
	}
	if len(a.tags) > 0 {
		if len(a.tags) > 20 {
			return nil, errc("bad_param", []string{"tags=[first 20 tags]"}, "param", "tags", "value", len(a.tags), "valid", "max 20")
		}
		set := map[string]bool{}
		var ds []string
		for _, tg := range a.tags {
			set[tg] = true
			ds = append(ds, "tag == "+strconv.Quote(tg))
		}
		desc := strings.Join(ds, " OR ")
		kids = append(kids, &filter.Pred{Desc: desc, ClassOnly: true, Fn: func(r *model.Record, _ *filter.Ctx) bool { return set[r.Tag] }})
		q.parts = append(q.parts, desc)
	}
	if strings.TrimSpace(a.filter) != "" {
		p, err := filter.Parse(a.filter)
		if err != nil {
			var fe *filter.Error
			if errors.As(err, &fe) {
				kv := []any{"col", fe.Col(), "msg", fe.Msg}
				if fe.Hint != "" {
					kv = append(kv, "hint", fe.Hint)
				}
				return nil, errc("bad_filter", nil, kv...)
			}
			return nil, err
		}
		if p.Root != nil {
			q.filterUsed = true
			root = p.Root
			kids = append(kids, p.Root)
			q.parts = append(q.parts, p.Root.String())
		}
		for _, n := range p.Notes {
			q.notes = append(q.notes, hdr("note", "code", "filter", "msg", n))
		}
		for _, h := range p.Hints {
			line := hdr("note", "code", "filter", "hint", h.Kind, "msg", h.Msg)
			if h.Fix != "" {
				line += nextClause(param("filter", h.Fix))
			}
			q.notes = append(q.notes, line)
		}
	}
	if a.minLevel != "" {
		lv, ok := filter.ParseLevel(a.minLevel)
		if !ok {
			return nil, errc("bad_param", []string{`min_level="info"`}, "param", "min_level", "value", a.minLevel, "valid", "verbose,debug,info,warn,error,fatal")
		}
		kids = append(kids, &filter.Level{Min: lv})
		q.parts = append(q.parts, "level >= "+lv.Name())
	}
	if a.from != "" {
		tm, err := timeparse.Parse("from", a.from, now)
		if err != nil {
			return nil, badTime("from", a.from)
		}
		q.fromNs = tm.UnixNano()
	}
	if a.to != "" {
		tm, err := timeparse.Parse("to", a.to, now)
		if err != nil {
			return nil, badTime("to", a.to)
		}
		q.toNs = tm.UnixNano()
	}
	// A top-level age: term also bounds the scan like from (issue #21): the
	// later of the two wins (both must hold). The age: predicate itself
	// stays, so results do not change; age: inside an OR is per-record only.
	ageSrc := ""
	if dur, src, ok := topLevelAge(root); ok {
		if b := now.Add(-dur).UnixNano(); q.fromNs == 0 || b > q.fromNs {
			q.fromNs, ageSrc = b, src
		}
	}
	// relative window (age:, from="5m"): ends at device now + relSlack
	// unless to is given or the call waits for new lines (issue #48)
	if (ageSrc != "" || timeparse.IsRelative(a.from)) && a.to == "" && !a.wait {
		q.toNs, q.relUpper = now.Add(relSlack).UnixNano(), true
	}
	if q.fromNs != 0 && q.toNs != 0 && q.fromNs >= q.toNs {
		if ageSrc != "" {
			return nil, errc("empty_window", []string{param("to", isoNs(q.fromNs+int64(time.Hour), q.loc))}, "age", ageSrc, "from", isoNs(q.fromNs, q.loc), "to", isoNs(q.toNs, q.loc))
		}
		return nil, errc("empty_window", []string{param("to", isoNs(q.fromNs+int64(time.Hour), q.loc))}, "from", isoNs(q.fromNs, q.loc), "to", isoNs(q.toNs, q.loc))
	}
	// age: alone needs no window part: the filter line says "age <= …"
	if ageOnly := ageSrc != "" && a.from == "" && (q.toNs == 0 || q.relUpper); !ageOnly && (q.fromNs != 0 || q.toNs != 0) {
		f, to := "...", "..."
		if q.fromNs != 0 {
			f = dateTime(q.fromNs, q.loc)
		}
		if q.relUpper {
			to = "dev_now+2s"
		} else if q.toNs != 0 {
			to = dateTime(q.toNs, q.loc)
		}
		q.parts = append(q.parts, fmt.Sprintf("device time in [%s, %s)", f, to))
	}
	if a.before != "" && a.after != "" {
		return nil, errc("bad_param", []string{param("before", a.before), param("after", a.after)}, "param", "before,after", "valid", "one of them")
	}
	for _, c := range []struct {
		name, v string
		dst     *uint64
	}{{"before", a.before, &q.beforeSeq}, {"after", a.after, &q.afterSeq}} {
		if c.v == "" {
			continue
		}
		cur, err := store.ParseCursor(c.v)
		if err != nil {
			return nil, errc("bad_cursor", []string{"logcat_status"}, "param", c.name, "msg", err.Error())
		}
		if !d.Matches(cur.Serial) {
			return nil, errc("cursor_device", []string{param("device", cur.Serial)}, "param", c.name, "cursor_device", cur.Serial)
		}
		last := d.Store.LastSeq()
		first := d.Store.FirstSeq()
		seq := cur.Seq
		if seq > last {
			q.notes = append(q.notes, hdr("note", "code", "cursor_ahead", "param", c.name, "using", "newest"))
			seq = last
		}
		if c.name == "after" && first > 0 && seq+1 < first {
			q.notes = append(q.notes, hdr("note", "code", "cursor_pruned", "boot", cur.Boot, "using", "oldest"))
		}
		*c.dst = seq
		if c.name == "before" && seq == 0 {
			*c.dst = 1 // nothing older
		}
	}
	switch len(kids) {
	case 0:
		q.pred = filter.Empty{}
	case 1:
		q.pred = kids[0]
	default:
		q.pred = &filter.And{Kids: kids}
	}
	return q, nil
}

// countBudget bounds how long a search keeps counting matches once its
// page (limit) is full (issue #20); the count is then a lower bound over
// the records scanned so far, and the output says which time span that
// covers. A variable for tests.
var countBudget = 100 * time.Millisecond

type scanResult struct {
	matches    []*model.Record // in scan order
	total      int             // matches counted (≥ len(matches))
	countDone  bool            // total is exact
	tagCount   map[string]int  // tags of the matches visited (tagN of them)
	tmplIdx    map[string]int  // message template -> index in tmplKeys/tmplNs of the same matches (summary head)
	tagN       int
	tmplKeys   []string
	tmplNs     []int
	tbuf       []byte
	byRange    bool              // total counted from the store's record range (no filter), tags only of the page
	firstNs    int64             // time of the first record scanned (desc: newest)
	lastNs     int64             // time of the last record scanned (where counting stopped)
	scannedTo  uint64            // asc: highest seq examined; desc: lowest seq examined
	upper      uint64            // exclusive upper bound used
	incomplete bool              // scan stopped by budget before covering the range
	jumps      []store.ClockJump // clock jumps inside the scanned seq range
	side       []int             // counted matches between consecutive jumps (len(jumps)+1)
}

// scan runs the query. desc=true collects the newest `limit` matches.
func (q *query) scan(limit int, desc bool) *scanResult {
	st := q.d.Store
	res := &scanResult{tagCount: map[string]int{}, tmplIdx: map[string]int{}, countDone: true}
	res.upper = q.beforeSeq
	if res.upper == 0 {
		res.upper = st.LastSeq() + 1
	}
	start := time.Now()
	var fullAt time.Time // when the page filled up
	byRange := q.countByRange()
	jr := q.jumpsIn(res.upper)
	res.jumps, res.side = jr.jumps, jr.side
	if len(res.jumps) > 0 {
		byRange = false // per-side counts need every match
	}
	// Issue #37: countable filters (tag, package, process and level terms only; uid is part of the class key, not a filter term) get
	// an exact total from per-block class counts, with no countBudget.
	countable := !byRange && filter.Countable(q.pred)
	var classIDs map[uint32]bool
	if countable {
		st.EnsureIndex()
		classIDs = st.MatchingClassIDs(func(r *model.Record) bool { return q.pred.Match(r, q.ectx) })
	}
	n := 0
	visit := func(r *model.Record) bool {
		n++
		res.scannedTo = r.Seq
		if n == 1 {
			res.firstNs = r.TimeNs
		}
		res.lastNs = r.TimeNs
		if n&4095 == 0 {
			el := time.Since(start)
			if !fullAt.IsZero() && time.Since(fullAt) > countBudget {
				if byRange { // exact count comes from the record range; only the digest sample ends here
					res.byRange = true
					return false
				}
				if countable { // exact total comes from the class index
					return false
				}
				res.countDone = false
				return false
			}
			if el > scanBudget {
				res.incomplete = true
				res.countDone = false
				return false
			}
		}
		if !q.inTime(r) || !q.pred.Match(r, q.ectx) {
			return true
		}
		res.total++
		res.addSide(r.Seq)
		if res.tagN < headSample { // the digest is of the first headSample matches
			res.tagCount[r.Tag]++
			res.tbuf = appendTemplate(res.tbuf[:0], r)
			if i, ok := res.tmplIdx[string(res.tbuf)]; ok { // no allocation for a known template
				res.tmplNs[i]++
			} else if len(res.tmplKeys) < headTemplates { // bounded: later new templates are not counted
				k := string(res.tbuf)
				res.tmplIdx[k] = len(res.tmplKeys)
				res.tmplKeys = append(res.tmplKeys, k)
				res.tmplNs = append(res.tmplNs, 1)
			}
			res.tagN++
		}
		if len(res.matches) < limit {
			res.matches = append(res.matches, r)
			if len(res.matches) == limit {
				fullAt = time.Now()
			}
		}
		if (byRange || countable) && len(res.matches) == limit && res.tagN >= headSample {
			if byRange {
				res.byRange = true
			}
			return false // the total comes from Count / CountMatch
		}
		return true
	}
	// only records in the time window, skipping history blocks wholly
	// outside it (and, when countable, blocks with no matching class);
	// never an order-based early stop (issues #21, #48, #37)
	if countable {
		st.ScanTimeClass(q.afterSeq, res.upper, desc, q.fromNs, q.toNs, classIDs, visit)
	} else {
		st.ScanTime(q.afterSeq, res.upper, desc, q.fromNs, q.toNs, visit)
	}
	if res.byRange {
		res.total = int(st.Count(q.afterSeq, res.upper))
		res.countDone = true
	} else if countable && !res.incomplete {
		match := func(r *model.Record) bool { return q.pred.Match(r, q.ectx) }
		res.total = int(st.CountMatch(q.afterSeq, res.upper, q.fromNs, q.toNs, classIDs, match))
		res.countDone = true
		// per-side counts across clock jumps: one CountMatch per side
		if len(res.jumps) > 0 {
			res.side = make([]int, len(res.jumps)+1)
			lo := q.afterSeq
			for i, j := range res.jumps {
				res.side[i] = int(st.CountMatch(lo, j.Seq, q.fromNs, q.toNs, classIDs, match))
				// CountMatch uses after < seq; seq == j.Seq belongs to the next side
				if j.Seq == 0 {
					lo = 0
				} else {
					lo = j.Seq - 1
				}
			}
			res.side[len(res.jumps)] = int(st.CountMatch(lo, res.upper, q.fromNs, q.toNs, classIDs, match))
		}
	}
	return res
}

// countByRange: every record in the seq range matches (no filter, no time
// window), so the exact total is the number of stored records in the range
// (Store.Count) and the scan can stop once the page is full (issue #20).
func (q *query) countByRange() bool {
	_, empty := q.pred.(filter.Empty)
	return empty && q.fromNs == 0 && q.toNs == 0
}

func (t *Tools) search(ctx context.Context, a searchArgs) (out string, err error) {
	if a.Limit == 0 {
		a.Limit = defaultLimit
	}
	if a.Limit < 1 || a.Limit > 1000 {
		return "", errc("bad_param", []string{"limit=1000"}, "param", "limit", "value", a.Limit, "valid", "1..1000")
	}
	if a.Context < 0 || a.Context > 20 {
		return "", errc("bad_param", []string{"context=20"}, "param", "context", "value", a.Context, "valid", "0..20")
	}
	if a.WaitMs < 0 || a.WaitMs > maxWaitMs {
		return "", errc("bad_param", []string{param("wait_ms", maxWaitMs)}, "param", "wait_ms", "value", a.WaitMs, "valid", "0.."+strconv.Itoa(maxWaitMs))
	}
	if a.View == "" {
		a.View = "lines"
	}
	if a.View != "lines" && a.View != "summary" {
		return "", errc("bad_param", []string{`view="lines"`, `view="summary"`}, "param", "view", "value", a.View)
	}
	if a.Stack == "" {
		a.Stack = "short"
	}
	if a.Stack != "short" && a.Stack != "full" {
		return "", errc("bad_param", []string{`stack="short"`, `stack="full"`}, "param", "stack", "value", a.Stack)
	}
	if a.WaitMs > 0 && a.After == "" {
		return "", errc("missing_param", []string{"logcat_status", param("wait_ms", 0)}, "param", "after", "needed_by", "wait_ms")
	}
	d, done, note, err := t.useAndWait(ctx, a.Device)
	defer done()
	if err != nil {
		return "", err
	}
	defer func() {
		if note != "" && err == nil { // after the header line: it stays the first line
			head, rest, _ := strings.Cut(out, "\n")
			out = head + "\n" + note + "\n" + rest
		}
	}()
	d.Store.Refresh()
	q, err := t.buildQuery(d, commonArgs{verbose: a.Verbose, filter: a.Filter, pkg: a.Package, minLevel: a.MinLevel, from: a.From, to: a.To, before: a.Before, after: a.After, tags: a.Tags, wait: a.WaitMs > 0})
	if err != nil {
		return "", err
	}
	if a.View == "summary" {
		return t.summary(q), nil
	}
	if a.After != "" {
		res := q.scan(a.Limit, false)
		waited := false
		if len(res.matches) == 0 && a.WaitMs > 0 {
			deadline := time.Now().Add(time.Duration(a.WaitMs) * time.Millisecond)
			for len(res.matches) == 0 && time.Now().Before(deadline) && ctx.Err() == nil {
				waited = true
				if !d.Store.Wait(ctx, max(q.afterSeq, res.upper-1), time.Until(deadline)) {
					break
				}
				q.afterSeq = max(q.afterSeq, res.upper-1)
				q.ectx.Now = d.Now()
				res = q.scan(a.Limit, false)
			}
		}
		return t.renderLines(q, res, a, false, waited), nil
	}
	res := q.scan(a.Limit, true)
	return t.renderLines(q, res, a, true, false), nil
}

// head is the first line of a search response (#85 P1): ok|empty, then
// key=value fields, ending with the clock fields and fmt.
func (t *Tools) head(q *query, word, view string, extra ...any) string {
	kv := append([]any{"view", view, "device", q.d.Serial}, extra...)
	if q.verbose {
		kv = append(kv, "tz", q.d.TZName())
	}
	kv = append(kv, clockKV(q.clock, q.verbose)...)
	kv = append(kv, "fmt", fmtVersion)
	return hdr(word, kv...) + "\n"
}

// clockNotes: the clock jumps inside the scanned range. Default: ONE line,
// only when a jump splits the matches (matches on both sides), for the
// newest such jump, with the cursor that shows only what is after it;
// verbose: every splitting jump (at most 3, newest first) in full, or the
// newest jump of the range when none splits. For a relative window, the
// lines excluded because they are dated after device now (issue #48).
func (t *Tools) clockNotes(b *strings.Builder, q *query, res *scanResult) {
	n := len(res.jumps)
	var show []int // jumps that split the matches, newest first
	sides := make([][2]int, n)
	for i := n - 1; i >= 0; i-- {
		for k, c := range res.side {
			if k <= i {
				sides[i][0] += c
			} else {
				sides[i][1] += c
			}
		}
		if sides[i][0] > 0 && sides[i][1] > 0 {
			show = append(show, i)
		}
	}
	counts := func(i int) (int, int) {
		if !res.countDone {
			return -1, -1
		}
		return sides[i][0], sides[i][1]
	}
	if len(show) > 0 {
		i := show[0]
		j := res.jumps[i]
		fmt.Fprintf(b, "clock_jumps=%d splitting=%d seq=%d delta_s=%s", n, len(show), j.Seq, val(secs(time.Duration(j.DeltaNs))))
		if bf, af := counts(i); bf >= 0 {
			fmt.Fprintf(b, " matches_before=%d matches_after=%d", bf, af)
		}
		b.WriteString(nextClause(param("after", t.cursor(q, j.Seq-1))) + "\n")
	}
	if q.verbose {
		list := show
		if len(list) == 0 && n > 0 {
			list = []int{n - 1}
		}
		for k, i := range list {
			if k == 3 {
				break
			}
			bf, af := counts(i)
			b.WriteString(jumpLines(res.jumps[i], q.loc, bf, af, t.cursor(q, res.jumps[i].Seq-1)))
		}
	}
	if q.relUpper {
		t.futureNote(b, q, res)
	}
}

// futureNote counts the matches (filter only, no window) dated at or after
// the relative window's end: lines logged before the device clock was set
// back. Only when a set-back is stored; bounded by scanBudget.
func (t *Tools) futureNote(b *strings.Builder, q *query, res *scanResult) {
	back := false
	for _, j := range q.d.Store.ClockJumps() {
		if j.DeltaNs < 0 && j.BeforeNs >= q.toNs {
			back = true
		}
	}
	if !back {
		return
	}
	start := time.Now()
	cnt, lo, hi, partial := 0, int64(0), int64(0), false
	q.d.Store.ScanTime(q.afterSeq, res.upper, true, q.toNs, 0, func(r *model.Record) bool {
		if cnt&1023 == 0 && time.Since(start) > scanBudget {
			partial = true
			return false
		}
		if r.TimeNs < q.toNs || !q.pred.Match(r, q.ectx) {
			return true
		}
		if cnt == 0 || r.TimeNs < lo {
			lo = r.TimeNs
		}
		if r.TimeNs > hi {
			hi = r.TimeNs
		}
		cnt++
		return true
	})
	if cnt == 0 {
		return
	}
	fmt.Fprintf(b, "clock_future matches=%d exact=%s range=%s/%s%s\n", cnt, val(!partial), isoNs(lo, q.loc), isoNs(hi, q.loc),
		nextClause(param("from", isoNs(lo, q.loc))+" "+param("to", isoNs(hi+int64(time.Second), q.loc))))
}

func (t *Tools) cursor(q *query, seq uint64) string {
	m := q.d.Store.Meta()
	boot := "0"
	for _, s := range m.Sessions {
		if s.FirstSeq <= seq+1 {
			boot = s.Boot
		}
	}
	return store.Cursor{Serial: q.d.Serial, Boot: boot, Seq: seq}.String()
}

// withContext expands shown matches by up to n same-pid lines on each side.
func (q *query) withContext(shown []*model.Record, n int) ([]*model.Record, map[uint64]bool) {
	isMatch := map[uint64]bool{}
	for _, r := range shown {
		isMatch[r.Seq] = true
	}
	if n == 0 {
		return shown, isMatch
	}
	all := map[uint64]*model.Record{}
	for _, r := range shown {
		all[r.Seq] = r
	}
	for _, m := range shown {
		for _, desc := range []bool{true, false} {
			got, scanned := 0, 0
			after, before := uint64(0), m.Seq
			if !desc {
				after, before = m.Seq, 0
			}
			q.d.Store.Scan(after, before, desc, func(r *model.Record) bool {
				scanned++
				if r.PID == m.PID {
					all[r.Seq] = r
					got++
				}
				return got < n && scanned < contextScanMx
			})
		}
	}
	out := make([]*model.Record, 0, len(all))
	for _, r := range all {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, isMatch
}

// headSample bounds the matches that the wide-search summary head (#85)
// is computed from (tags and message templates); sampled= says how many.
// When the sample is smaller than the matches, percentages carry a "~".
const headSample = 2000

// headTemplates bounds the distinct message templates the digest keeps (memory
// and allocations per call); templates first seen after that are not counted.
const headTemplates = 256

// summaryHead is the one-line digest of a wide search (#85): the top tags
// and message templates of the matches, so one call tells what the hidden
// matches are. At most headCap chars: msgs go first, then tags.
const headCap = 300

func summaryHead(res *scanResult) string {
	of := res.tagN
	if of == 0 {
		return ""
	}
	type kv struct {
		k string
		v int
	}
	// top keeps the n largest (count desc, key asc) of the entries each yields,
	// without sorting or copying all of them.
	top := func(each func(yield func(string, int)), n int) []kv {
		l := make([]kv, 0, n+1)
		each(func(k string, v int) {
			if len(l) == n && !(v > l[n-1].v || v == l[n-1].v && k < l[n-1].k) {
				return
			}
			i := len(l)
			l = append(l, kv{k, v})
			for ; i > 0 && (v > l[i-1].v || v == l[i-1].v && k < l[i-1].k); i-- {
				l[i] = l[i-1]
			}
			l[i] = kv{k, v}
			if len(l) > n {
				l = l[:n]
			}
		})
		return l
	}
	approx := ""
	if res.total > of {
		approx = "~"
	}
	pct := func(v int) string {
		p := (100*v + of/2) / of
		if p < 1 {
			return approx + "<1%"
		}
		return approx + strconv.Itoa(p) + "%"
	}
	tl := top(func(y func(string, int)) {
		for k, v := range res.tagCount {
			y(k, v)
		}
	}, 5)
	var ml []kv
	for _, m := range top(func(y func(string, int)) {
		for i, k := range res.tmplKeys {
			y(k, res.tmplNs[i])
		}
	}, 6) {
		if m.v > 1 {
			ml = append(ml, m)
		}
	}
	build := func(nt, nm int) string {
		var b strings.Builder
		top3 := 0
		for _, t := range tl[:min(3, len(tl))] {
			top3 += t.v
		}
		fmt.Fprintf(&b, "summary sampled=%d top3_share=%s tags=", of, pct(top3))
		for i, t := range tl[:min(nt, len(tl))] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(noSpace(t.k) + ":" + pct(t.v))
		}
		if nm > 0 && len(ml) > 0 {
			b.WriteString(" msgs=")
			for i, m := range ml[:min(nm, len(ml))] {
				if i > 0 {
					b.WriteByte(',')
				}
				t := m.k
				if len(t) > 60 {
					t = t[:cutAt(t, 60)] + "..."
				}
				fmt.Fprintf(&b, "%dx%s", m.v, strconv.Quote(t))
			}
		}
		return b.String()
	}
	for nm := len(ml); nm >= 0; nm-- {
		if s := build(len(tl), nm); len(s) <= headCap {
			return s + "\n"
		}
	}
	for nt := len(tl) - 1; nt >= 1; nt-- {
		if s := build(nt, 0); len(s) <= headCap {
			return s + "\n"
		}
	}
	s := build(1, 0)
	return s[:cutAt(s, headCap)] + "\n"
}

func (t *Tools) renderLines(q *query, res *scanResult, a searchArgs, desc, waited bool) string {
	var b strings.Builder
	// choose shown matches within the output budget
	short := a.Stack != "full"
	cache := map[*model.Record]shortRes{}
	est := &lineWriter{loc: q.loc, short: short, cache: cache}
	if short {
		est.seen = map[string]int64{} // a repeated stack costs a reference line here too
	}
	matches := res.matches
	budget := outputBudget - 2500
	shown := 0
	size := 0
	for _, r := range matches { // scan order: desc = newest first, asc = oldest first
		l := len(est.format(r, "")) + 1
		if a.Context > 0 {
			l *= 1 + min(a.Context, 4) // rough estimate
		}
		if shown > 0 && size+l > budget {
			break
		}
		size += l
		shown++
	}
	capped := shown < len(matches)
	sel := append([]*model.Record(nil), matches[:shown]...)
	if desc {
		for i, j := 0, len(sel)-1; i < j; i, j = i+1, j-1 {
			sel[i], sel[j] = sel[j], sel[i]
		}
	}
	lines, isMatch := q.withContext(sel, a.Context)
	render := func(recs []*model.Record) *lineWriter {
		w := &lineWriter{loc: q.loc, short: short, cache: cache}
		if short {
			w.seen = map[string]int64{}
		}
		for _, r := range recs {
			w.add(r, !isMatch[r.Seq])
		}
		w.flushRepeat()
		return w
	}
	w := render(lines)
	// trim further if context made it too large
	trimmed := false
	if w.size() > budget && len(w.lines) > 1 {
		trimmed = true
		if !short {
			for len(w.lines) > 1 && w.size() > budget {
				if desc {
					w.lines = w.lines[1:]
				} else {
					w.lines = w.lines[:len(w.lines)-1]
				}
			}
		} else {
			// drop whole records, so that a "same frames as" reference
			// never points at a line that was cut: the fewest that fit
			drop := func(k int) []*model.Record {
				if desc {
					return lines[k:]
				}
				return lines[:len(lines)-k]
			}
			lo, hi := 1, len(lines)-1
			for lo < hi {
				mid := (lo + hi) / 2
				if render(drop(mid)).size() > budget {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			w = render(drop(lo))
		}
	}

	// ---- header
	rest := res.total - len(sel)
	order := "arrival"
	var extra []any
	if len(sel) == 0 {
		extra = []any{"matched", res.total, "scanned", 0, "exact", res.countDone}
	} else {
		extra = []any{"matched", res.total, "exact", res.countDone}
	}
	if !res.countDone && res.lastNs != 0 {
		key := "counted_from"
		if !desc {
			key = "counted_to"
		}
		extra = append(extra, key, isoNs(res.lastNs, q.loc))
	}
	var hints string
	word := "ok"
	if len(sel) == 0 {
		word = "empty"
		scanned := 0
		if !waited {
			scanned, hints = t.zeroHints(q)
		}
		extra[3] = scanned
		if st := q.d.Store.Stats(); st.Records > 0 {
			extra = append(extra, "range", isoNs(st.OldestNs, q.loc)+"/"+isoNs(st.NewestNs, q.loc))
		}
		extra = append(extra, "order", order)
		if waited {
			extra = append(extra, "waited_ms", a.WaitMs)
		}
	} else {
		lo, hi := sel[0].TimeNs, sel[0].TimeNs
		for _, r := range sel {
			lo, hi = min(lo, r.TimeNs), max(hi, r.TimeNs)
		}
		extra = append(extra, "shown", len(sel), "order", order, "range", isoNs(lo, q.loc)+"/"+isoNs(hi, q.loc))
	}
	b.WriteString(t.head(q, word, "lines", extra...))
	if q.filterUsed || q.verbose {
		b.WriteString(hdr("filter", "parsed", q.interp()) + "\n")
	}
	for _, n := range q.notes {
		b.WriteString(n + "\n")
	}
	t.rangeNotes(&b, q, sel)
	t.clockNotes(&b, q, res)
	if rest > 0 && len(sel) > 0 {
		b.WriteString(summaryHead(res))
	}
	if q.verbose && len(sel) > 0 {
		b.WriteString("columns: time level pid/tid process tag: message\n")
	}
	if len(sel) == 0 {
		b.WriteString(hints)
		if waited {
			b.WriteString(hdr("wait", "ms", a.WaitMs) + nextClause(param("after", t.cursor(q, res.upper-1))+" "+param("wait_ms", a.WaitMs)) + "\n")
		}
	}
	for _, l := range w.lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if w.stats.shortened > 0 {
		b.WriteString(hdr("truncated", "stack_frames", w.stats.hidden, "stacks", w.stats.shortened, "exact", 1) + nextClause(`stack="full"`) + "\n")
	}
	if trimmed {
		what := "lines"
		if a.Context > 0 {
			what = "context"
		}
		b.WriteString(hdr("truncated", what, len(w.lines), "exact", 1, "cap", "output_30000") + nextClause(param("limit", max(1, len(sel)/2))) + "\n")
	}
	capKV := []any{}
	if capped {
		capKV = append(capKV, "cap", "output_30000")
	}
	if desc {
		older := res.scannedTo
		if len(sel) > 0 {
			older = sel[0].Seq
		}
		if len(sel) > 0 && (rest > 0 || !res.countDone) {
			b.WriteString(truncLine("older", rest, res.countDone, capKV, param("before", t.cursor(q, older)), `view="summary"`))
		}
		b.WriteString(hdr("newer") + ": " + param("after", t.cursor(q, res.upper-1)) + "\n")
	} else {
		next := res.upper - 1
		if len(sel) > 0 && (rest > 0 || !res.countDone) {
			next = sel[len(sel)-1].Seq
			b.WriteString(truncLine("newer", rest, res.countDone, capKV, param("after", t.cursor(q, next))))
		}
		b.WriteString(hdr("newer") + ": " + param("after", t.cursor(q, next)) + "\n")
	}
	if res.incomplete {
		b.WriteString(truncLine("scan_stopped_ms", int(scanBudget.Milliseconds()), false, nil, param("before", t.cursor(q, res.scannedTo))))
	}
	return b.String()
}

func (t *Tools) rangeNotes(b *strings.Builder, q *query, sel []*model.Record) {
	m := q.d.Store.Meta()
	stats := q.d.Store.Stats()
	if q.fromNs != 0 && stats.OldestNs != 0 && q.fromNs < stats.OldestNs && len(q.d.Store.ClockJumps()) == 0 {
		b.WriteString(hdr("note", "code", "history_starts", "oldest", isoNs(stats.OldestNs, q.loc)) + "\n")
	}
	lo, hi := q.fromNs, q.toNs
	if len(sel) > 0 {
		if lo == 0 {
			lo = sel[0].TimeNs
		}
		if hi == 0 {
			hi = sel[len(sel)-1].TimeNs
		}
	}
	if lo == 0 && hi == 0 {
		return
	}
	for _, g := range m.Gaps {
		gto := g.ToNs
		if gto <= 0 {
			gto = 1 << 62
		}
		if (hi == 0 || g.FromNs < hi) && gto > lo {
			b.WriteString(gapLine(g, q.loc) + "\n")
		}
	}
}

// zeroHints explains the most likely reasons for an empty result as
// "hint code=... next: ..." lines, and returns how many records it scanned
// (the empty header's scanned=, so "no data in range" differs from "the
// filter matched nothing").
func (t *Tools) zeroHints(q *query) (int, string) {
	var b strings.Builder
	stats := q.d.Store.Stats()
	if stats.Records == 0 {
		return 0, hdr("hint", "code", "no_records") + nextClause("logcat_status") + "\n"
	}
	if q.toNs != 0 && stats.OldestNs != 0 && q.toNs <= stats.OldestNs {
		b.WriteString(hdr("hint", "code", "window_before_oldest", "oldest", isoNs(stats.OldestNs, q.loc)) + nextClause(param("from", isoNs(stats.OldestNs, q.loc))) + "\n")
	}
	tags := map[string]int{}
	pkgs := map[string]int{}
	own := procspkg.NameOwner(q.d.Packages()) // dotted process names (issue #41)
	n := 0
	q.d.Store.ScanTime(q.afterSeq, q.beforeSeq, true, q.fromNs, q.toNs, func(r *model.Record) bool {
		n++
		if q.inTime(r) {
			tags[r.Tag]++
			if p := r.Package; p != "" {
				pkgs[p]++
			} else if r.Process != "" {
				pkgs[own(r.Process)]++
			}
		}
		return n < 300000
	})
	if q.pkg != "" && pkgs[q.pkg] == 0 {
		var names []string
		for p := range pkgs {
			names = append(names, p)
		}
		kv := []any{"code", "no_logs_for_package", "package", q.pkg}
		if pk := q.d.Packages(); len(pk) > 0 {
			inst := false
			for _, p := range allPkgs(pk) {
				if p == q.pkg {
					inst = true
				}
			}
			kv = append(kv, "installed", inst)
		}
		sim := similar(q.pkg, names)
		if len(sim) > 0 {
			kv = append(kv, "similar", strings.Join(sim, ","))
		}
		if top := topKeys(pkgs, 8); top != "" {
			kv = append(kv, "packages_in_range", top)
		}
		alt := []string{`view="summary"`}
		if len(sim) > 0 {
			alt = []string{param("package", sim[0])}
		}
		b.WriteString(hdr("hint", kv...) + nextClause(alt...) + "\n")
	}
	for _, tg := range q.tags {
		if tags[tg] == 0 {
			var near []string
			for k := range tags {
				if strings.EqualFold(k, tg) {
					near = append(near, k)
				}
			}
			if len(near) == 0 {
				var names []string
				for k := range tags {
					names = append(names, k)
				}
				near = similar(tg, names)
			}
			kv := []any{"code", "no_logs_for_tag", "tag", tg}
			alt := []string{`view="summary"`}
			if len(near) > 0 {
				kv = append(kv, "similar", strings.Join(near, ","))
				alt = []string{`tags=[` + strconv.Quote(near[0]) + `]`}
			}
			b.WriteString(hdr("hint", kv...) + nextClause(alt...) + "\n")
		}
	}
	if q.filterUsed && b.Len() == 0 {
		b.WriteString(hdr("hint", "code", "filter_no_match") + nextClause(`view="summary"`, `filter=""`) + "\n")
	}
	if b.Len() == 0 {
		b.WriteString(hdr("hint", "code", "no_match_in_range") + nextClause(`view="summary"`, `min_level="verbose"`) + "\n")
	}
	return n, b.String()
}

// topKeys: "a:12,b:3" for the n most frequent keys.
func topKeys(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v || l[i].v == l[j].v && l[i].k < l[j].k })
	var parts []string
	for i := 0; i < len(l) && i < n; i++ {
		parts = append(parts, noSpace(l[i].k)+":"+strconv.Itoa(l[i].v))
	}
	return strings.Join(parts, ",")
}

// summary implements view="summary".
func (t *Tools) summary(q *query) string {
	levels := map[int]int{}
	tags := map[string]int{}
	procs := map[string]int{}
	tmpl := map[string]int{}
	var errs []*model.Record
	total := 0
	var oldest, newest int64
	start := time.Now()
	incomplete := false
	n := 0
	res := q.jumpsIn(q.beforeSeq)
	st := q.d.Store
	countable := filter.Countable(q.pred)
	var classIDs map[uint32]bool
	match := func(r *model.Record) bool { return q.pred.Match(r, q.ectx) }
	if countable {
		st.EnsureIndex()
		classIDs = st.MatchingClassIDs(match)
		// exact total / levels / tags / processes from class counts (issue #37)
		total, levels, tags, procs = st.AggClasses(q.afterSeq, q.beforeSeq, q.fromNs, q.toNs, classIDs, match)
	}
	visit := func(r *model.Record) bool {
		n++
		if n&4095 == 0 && time.Since(start) > scanBudget {
			incomplete = true
			return false
		}
		if !q.inTime(r) || !match(r) {
			return true
		}
		if !countable {
			total++
			levels[r.Level.Rank()]++
			tags[r.Tag]++
			procs[procLabel(r)]++
		}
		res.addSide(r.Seq)
		// stored order is not time order (clock changes): true min / max
		if newest == 0 || r.TimeNs > newest {
			newest = r.TimeNs
		}
		if oldest == 0 || r.TimeNs < oldest {
			oldest = r.TimeNs
		}
		tmpl[template(r)]++
		if r.Level.Rank() >= 5 && len(errs) < 5 {
			errs = append(errs, r)
		}
		return true
	}
	// countable: still scan under budget for msgs / latest errors / time range
	// ("newest part"); levels/tags/procs/total already exact from AggClasses
	if countable {
		st.ScanTimeClass(q.afterSeq, q.beforeSeq, true, q.fromNs, q.toNs, classIDs, visit)
	} else {
		st.ScanTime(q.afterSeq, q.beforeSeq, true, q.fromNs, q.toNs, visit)
	}
	var b strings.Builder
	// countable: total/levels/tags/procs are exact from the index even when
	// the msgs/errors scan hit the budget (issue #37 decision 3a)
	exact := !incomplete || countable
	extra := []any{"matched", total, "exact", exact}
	if total == 0 {
		st := q.d.Store.Stats()
		extra = []any{"matched", total, "scanned", n, "exact", exact}
		if st.Records > 0 {
			extra = append(extra, "range", isoNs(st.OldestNs, q.loc)+"/"+isoNs(st.NewestNs, q.loc))
		}
	} else {
		extra = append(extra, "order", "arrival", "range", isoNs(oldest, q.loc)+"/"+isoNs(newest, q.loc))
	}
	word := "ok"
	if total == 0 {
		word = "empty"
	}
	b.WriteString(t.head(q, word, "summary", extra...))
	if q.filterUsed || q.verbose {
		b.WriteString(hdr("filter", "parsed", q.interp()) + "\n")
	}
	for _, n := range q.notes {
		b.WriteString(n + "\n")
	}
	t.rangeNotes(&b, q, nil)
	res.countDone = exact
	t.clockNotes(&b, q, res)
	if total == 0 {
		_, hints := t.zeroHints(q)
		b.WriteString(hints)
		return b.String()
	}
	fmt.Fprintf(&b, "levels V=%d D=%d I=%d W=%d E=%d F=%d\n", levels[1], levels[2], levels[3], levels[4], levels[5], levels[6])
	b.WriteString("tags " + topKeys(tags, 10))
	if len(tags) > 10 {
		fmt.Fprintf(&b, " more=%d", len(tags)-10)
	}
	b.WriteString("\nprocs " + topKeys(procs, 5) + "\n")
	type kv struct {
		k string
		v int
	}
	var tl []kv
	for k, v := range tmpl {
		if v > 1 {
			tl = append(tl, kv{k, v})
		}
	}
	sort.Slice(tl, func(i, j int) bool { return tl[i].v > tl[j].v || tl[i].v == tl[j].v && tl[i].k < tl[j].k })
	if len(tl) > 0 {
		var parts []string
		for i := 0; i < len(tl) && i < 5; i++ {
			parts = append(parts, fmt.Sprintf("%dx%s", tl[i].v, strconv.Quote(tl[i].k)))
		}
		b.WriteString("msgs " + strings.Join(parts, ",") + "\n")
	}
	// crashes in range: counts and the call that lists them
	var crs, anrs []string
	own := procspkg.NameOwner(q.d.Packages())
	for _, c := range q.d.Crashes() {
		if (q.fromNs != 0 && c.TimeNs < q.fromNs) || (q.toNs != 0 && c.TimeNs >= q.toNs) || c.TimeNs < oldest || c.TimeNs > newest+int64(time.Second) {
			continue
		}
		if q.pkg != "" && c.Package != q.pkg && own(c.Process) != q.pkg {
			continue
		}
		if c.Kind == "anr" {
			anrs = append(anrs, c.ID)
		} else {
			crs = append(crs, c.ID)
		}
	}
	if len(crs)+len(anrs) > 0 {
		fmt.Fprintf(&b, "crashes=%d anrs=%d%s\n", len(crs), len(anrs), nextClause("logcat_crashes "+param("from", isoNs(oldest, q.loc))+" "+param("to", isoNs(newest+int64(time.Second), q.loc))))
	}
	if len(errs) > 0 {
		fmt.Fprintf(&b, "latest_errors=%d\n", len(errs))
		for i := len(errs) - 1; i >= 0; i-- {
			r := errs[i]
			first := strings.SplitN(r.Msg, "\n", 2)[0]
			if len(first) > 200 {
				first = first[:cutAt(first, 200)] + "..."
			}
			b.WriteString(formatLine(r, q.loc, "", first) + "\n")
		}
	}
	if incomplete {
		b.WriteString(truncLine("scan_stopped_ms", int(scanBudget.Milliseconds()), false, nil, param("from", isoNs(newest-int64(time.Hour), q.loc)))) // narrow
	}
	next := []string{`view="lines"`}
	if top := topKeys(tags, 1); top != "" {
		next = []string{`view="lines" ` + `tags=[` + strconv.Quote(strings.SplitN(top, ":", 2)[0]) + `]`}
	}
	if len(crs) > 0 {
		next = append(next, "logcat_crashes "+param("id", crs[0]))
	}
	b.WriteString(strings.TrimPrefix(nextClause(next...), " ") + "\n")
	return b.String()
}

func capList(l []string, n int) []string {
	if len(l) > n {
		return append(l[:n:n], fmt.Sprintf("...%d more", len(l)-n))
	}
	return l
}

func shortHeadline(h string) string {
	h = strings.SplitN(h, "\n", 2)[0]
	if i := strings.Index(h, ": "); i > 0 && strings.Contains(h[:i], ".") {
		h = h[:i]
	}
	if len(h) > 100 {
		h = h[:cutAt(h, 100)] + "..."
	}
	return h
}

// jumpsIn: a scanResult holding the clock jumps inside (afterSeq, upper)
// (upper 0: up to the newest line), ready for per-side counting.
func (q *query) jumpsIn(upper uint64) *scanResult {
	st := q.d.Store
	if upper == 0 {
		upper = st.LastSeq() + 1
	}
	res := &scanResult{upper: upper, countDone: true}
	for _, j := range st.ClockJumps() {
		if j.Seq > q.afterSeq && j.Seq < upper {
			res.jumps = append(res.jumps, j)
		}
	}
	if len(res.jumps) > 0 {
		res.side = make([]int, len(res.jumps)+1)
	}
	return res
}

// addSide counts a match on its side of the clock jumps.
func (res *scanResult) addSide(seq uint64) {
	if res.side != nil {
		res.side[sort.Search(len(res.jumps), func(i int) bool { return res.jumps[i].Seq > seq })]++
	}
}
