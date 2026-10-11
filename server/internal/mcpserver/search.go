package mcpserver

import (
	"context"
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
	clock      device.ClockInfo // device now used for relative values
	relUpper   bool             // toNs is device now + relSlack (relative window)
}

func (q *query) interp() string {
	if len(q.parts) == 0 {
		return "(none — all lines)"
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
func hostRejected(param, v string, ci device.ClockInfo) error {
	if param == "age" {
		return errf("age:host:%s: age: always counts back from device now (%s); host time is not supported", v, ci.Now.Format("2006-01-02 15:04:05"))
	}
	return errf("%s: host time is not supported: from/to/age: use device time (the Android device's clock, as printed in logcat lines). Device now is %s; host now is %s. For \"what happens from now on\" whatever the clocks, take the cursor from logcat_status and call logcat_search(after=<cursor>).",
		param, ci.Now.Format("2006-01-02 15:04:05"), hostTime(ci.HostNow))
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
	q := &query{d: d, loc: d.Loc(), ectx: &filter.Ctx{Now: now, Loc: d.Loc()}, pkg: a.pkg, tags: a.tags, clock: ci}
	var kids []filter.Node
	var root filter.Node // the parsed filter
	if a.pkg != "" {
		pkg := strings.TrimSpace(a.pkg)
		q.pkg = pkg
		su := newSharedUID(pkg, d.Packages(), d.Procs)
		kids = append(kids, &filter.Pred{Desc: "package == " + pkg, Fn: func(r *model.Record, _ *filter.Ctx) bool { return pkgMatches(r, pkg) || su.line(r.UID, r.Process) }})
		if su != nil {
			q.notes = append(q.notes, su.note())
		}
		q.parts = append(q.parts, "package == "+pkg)
	}
	if len(a.tags) > 0 {
		if len(a.tags) > 20 {
			return nil, errf("tags: at most 20 tags (got %d)", len(a.tags))
		}
		set := map[string]bool{}
		var ds []string
		for _, tg := range a.tags {
			set[tg] = true
			ds = append(ds, "tag == "+strconv.Quote(tg))
		}
		desc := strings.Join(ds, " OR ")
		kids = append(kids, &filter.Pred{Desc: desc, Fn: func(r *model.Record, _ *filter.Ctx) bool { return set[r.Tag] }})
		q.parts = append(q.parts, desc)
	}
	if strings.TrimSpace(a.filter) != "" {
		p, err := filter.Parse(a.filter)
		if err != nil {
			return nil, err
		}
		if p.Root != nil {
			q.filterUsed = true
			root = p.Root
			kids = append(kids, p.Root)
			q.parts = append(q.parts, p.Root.String())
		}
		for _, n := range p.Notes {
			q.notes = append(q.notes, n)
		}
	}
	if a.minLevel != "" {
		lv, ok := filter.ParseLevel(a.minLevel)
		if !ok {
			return nil, errf("min_level=%q is not a level. Use one of verbose debug info warn error fatal.", a.minLevel)
		}
		kids = append(kids, &filter.Level{Min: lv})
		q.parts = append(q.parts, "level >= "+lv.Name())
	}
	if a.from != "" {
		tm, err := timeparse.Parse("from", a.from, now)
		if err != nil {
			return nil, errf("%v", err)
		}
		q.fromNs = tm.UnixNano()
	}
	if a.to != "" {
		tm, err := timeparse.Parse("to", a.to, now)
		if err != nil {
			return nil, errf("%v", err)
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
			return nil, errf("age:%s starts at %s, which is not before to (%s): the device-time window is empty.", ageSrc, dateTime(q.fromNs, q.loc), dateTime(q.toNs, q.loc))
		}
		return nil, errf("from (%s) is not before to (%s): the device-time window is empty.", dateTime(q.fromNs, q.loc), dateTime(q.toNs, q.loc))
	}
	// age: alone needs no window part: the filter line says "age <= …"
	if ageOnly := ageSrc != "" && a.from == "" && (q.toNs == 0 || q.relUpper); !ageOnly && (q.fromNs != 0 || q.toNs != 0) {
		f, to := "…", "…"
		if q.fromNs != 0 {
			f = dateTime(q.fromNs, q.loc)
		}
		if q.relUpper {
			to = "device now + 2 s"
		} else if q.toNs != 0 {
			to = dateTime(q.toNs, q.loc)
		}
		q.parts = append(q.parts, fmt.Sprintf("device time in [%s, %s)", f, to))
	}
	if a.before != "" && a.after != "" {
		return nil, errf("pass either before or after, not both (before pages to older matches, after to newer ones).")
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
			return nil, errf("%s: %v", c.name, err)
		}
		if !d.Matches(cur.Serial) {
			return nil, errf("%s cursor belongs to device %s; pass device=%q or take a cursor from this device.", c.name, cur.Serial, cur.Serial)
		}
		last := d.Store.LastSeq()
		first := d.Store.FirstSeq()
		seq := cur.Seq
		if seq > last {
			q.notes = append(q.notes, fmt.Sprintf("%s cursor is ahead of stored data (store was reset?); using the newest position instead.", c.name))
			seq = last
		}
		if c.name == "after" && first > 0 && seq+1 < first {
			q.notes = append(q.notes, fmt.Sprintf("cursor refers to boot %s data that has been pruned; showing from oldest available instead.", cur.Boot))
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
	tagN       int
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
	res := &scanResult{tagCount: map[string]int{}, countDone: true}
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
	n := 0
	// only records in the time window, skipping history blocks wholly
	// outside it; never an order-based early stop (issues #21, #48)
	st.ScanTime(q.afterSeq, res.upper, desc, q.fromNs, q.toNs, func(r *model.Record) bool {
		n++
		res.scannedTo = r.Seq
		if n == 1 {
			res.firstNs = r.TimeNs
		}
		res.lastNs = r.TimeNs
		if n&4095 == 0 {
			el := time.Since(start)
			if !fullAt.IsZero() && time.Since(fullAt) > countBudget {
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
		res.tagCount[r.Tag]++
		res.tagN++
		if len(res.matches) < limit {
			res.matches = append(res.matches, r)
			if len(res.matches) == limit {
				fullAt = time.Now()
				if byRange {
					res.byRange = true
					return false // the total comes from the record range
				}
			}
		}
		return true
	})
	if res.byRange {
		res.total = int(st.Count(q.afterSeq, res.upper))
		res.countDone = true
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
		return "", errf("limit must be 1..1000 (got %d)", a.Limit)
	}
	if a.Context < 0 || a.Context > 20 {
		return "", errf("context must be 0..20 (got %d)", a.Context)
	}
	if a.WaitMs < 0 || a.WaitMs > maxWaitMs {
		return "", errf("wait_ms must be 0..%d (got %d); call again with the same after cursor to keep waiting", maxWaitMs, a.WaitMs)
	}
	if a.View == "" {
		a.View = "lines"
	}
	if a.View != "lines" && a.View != "summary" {
		return "", errf("view must be \"lines\" or \"summary\" (got %q)", a.View)
	}
	if a.Stack == "" {
		a.Stack = "short"
	}
	if a.Stack != "short" && a.Stack != "full" {
		return "", errf("stack must be \"short\" or \"full\" (got %q)", a.Stack)
	}
	if a.WaitMs > 0 && a.After == "" {
		return "", errf("wait_ms needs an after cursor: call logcat_status to get `cursor`, then logcat_search(after=<cursor>, wait_ms=%d)", a.WaitMs)
	}
	d, done, note, err := t.useAndWait(ctx, a.Device)
	defer done()
	if err != nil {
		return "", err
	}
	defer func() {
		if note != "" && err == nil {
			out = "note: " + note + "\n" + out
		}
	}()
	d.Store.Refresh()
	q, err := t.buildQuery(d, commonArgs{filter: a.Filter, pkg: a.Package, minLevel: a.MinLevel, from: a.From, to: a.To, before: a.Before, after: a.After, tags: a.Tags, wait: a.WaitMs > 0})
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

func (t *Tools) header(q *query, firstNs int64) string {
	ns := firstNs
	if ns == 0 {
		ns = q.clock.Now.UnixNano()
	}
	return fmt.Sprintf("device %s · device time %s, date %s · %s\n", q.d.Serial, tzLabel(q.loc, q.d.TZName(), time.Unix(0, ns)), dateOf(ns, q.loc), clockLine(q.clock))
}

// clockNotes: the clock jumps inside the scanned range (at most 3, newest
// first, with matches on each side) and, for a relative window, the lines
// excluded because they are dated after device now (issue #48).
func (t *Tools) clockNotes(b *strings.Builder, q *query, res *scanResult) {
	// the jumps that split the matches (matches on both sides); with
	// none, the newest jump in the range for context. At most 3, newest
	// first.
	n := len(res.jumps)
	var show []int
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
	split := len(show)
	if split == 0 && n > 0 {
		show = []int{n - 1}
	}
	if n > 1 {
		what := fmt.Sprintf("%d split the matches", split)
		if split > 3 {
			what += "; the newest 3"
		} else if split == 0 {
			what = "none splits the matches; the newest"
		}
		fmt.Fprintf(b, "clock: %d device clock jumps in the scanned range; %s:\n", n, what)
	}
	for k, i := range show {
		if k == 3 {
			break
		}
		before, after := sides[i][0], sides[i][1]
		if !res.countDone {
			before, after = -1, -1
		}
		fmt.Fprint(b, jumpLines(res.jumps[i], q.loc, before, after, t.cursor(q, res.jumps[i].Seq-1)))
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
	c := commas(cnt)
	if partial {
		c = "≥" + c
	}
	fmt.Fprintf(b, "clock: %s matching lines are dated after device now (logged before the device clock was set back), device time %s – %s; not in this relative window · from=%q to=%q shows them\n",
		c, dateTime(lo, q.loc), dateTime(hi, q.loc), dateTime(lo, q.loc), dateTime(hi+int64(time.Second), q.loc))
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

	first := int64(0)
	if len(sel) > 0 {
		first = sel[0].TimeNs
	}
	b.WriteString(t.header(q, first))
	b.WriteString("filter parsed as: " + q.interp() + "\n")
	for _, n := range q.notes {
		b.WriteString("note: " + n + "\n")
	}
	t.rangeNotes(&b, q, sel)
	t.clockNotes(&b, q, res)
	totalStr := commas(res.total)
	partial := !res.countDone
	if partial {
		totalStr = "≥" + totalStr
	}
	switch {
	case len(sel) == 0:
		if waited {
			fmt.Fprintf(&b, "no new matches within %d ms; call again with the same after cursor to keep waiting.\n", a.WaitMs)
		} else {
			b.WriteString("matched 0 lines\n")
			b.WriteString(t.zeroHints(q))
		}
	case desc:
		fmt.Fprintf(&b, "matched %s lines%s; showing newest %d (%s–%s)\n", totalStr, countScope(res, q.loc, desc), len(sel), clock(sel[0].TimeNs, q.loc), clock(sel[len(sel)-1].TimeNs, q.loc))
	default:
		fmt.Fprintf(&b, "matched %s new lines%s; showing oldest %d (%s–%s)\n", totalStr, countScope(res, q.loc, desc), len(sel), clock(sel[0].TimeNs, q.loc), clock(sel[len(sel)-1].TimeNs, q.loc))
	}
	b.WriteString(countFields(res, q.loc, desc))
	if len(sel) > 0 {
		b.WriteString("format: time level pid/tid process tag: message\n")
		for _, l := range w.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if w.stats.shortened > 0 {
		fmt.Fprintf(&b, "stacks: %d shortened (%s frames hidden) · stack=\"full\" shows every frame\n", w.stats.shortened, commas(w.stats.hidden))
	}
	if capped {
		fmt.Fprintf(&b, "[output cap: only %d of the %d matches asked for fit in ~30k chars · view=\"summary\" gives an overview; or narrow from/to or the filter]\n", len(sel), len(matches))
	}
	if trimmed && a.Context > 0 {
		b.WriteString("[output cap: context lines cut to fit in ~30k chars · use a smaller context or limit, or narrow from/to or the filter]\n")
	} else if trimmed {
		b.WriteString("[output cap: lines cut to fit in ~30k chars · view=\"summary\" gives an overview; or narrow from/to or the filter]\n")
	}
	rest := res.total - len(sel)
	if desc {
		if rest > 0 || !res.countDone {
			fmt.Fprintf(&b, "[truncated: %s older matches not shown · view=\"summary\" shows what the whole range contains]\n", map[bool]string{true: commas(rest), false: "≥" + commas(rest)}[res.countDone])
		}
		older := res.scannedTo
		if len(sel) > 0 {
			older = sel[0].Seq
		}
		fmt.Fprintf(&b, "more: before=%q  ·  newer/wait: after=%q\n", t.cursor(q, older), t.cursor(q, res.upper-1))
	} else {
		next := res.upper - 1
		if rest > 0 || !res.countDone {
			next = sel[len(sel)-1].Seq
			fmt.Fprintf(&b, "[truncated: %s newer matches not shown]\n", map[bool]string{true: commas(rest), false: "≥" + commas(rest)}[res.countDone])
		}
		fmt.Fprintf(&b, "more/wait: after=%q\n", t.cursor(q, next))
	}
	if res.incomplete {
		fmt.Fprintf(&b, "note: scan stopped after %s to stay responsive (range not fully searched); continue: before=%q, or narrow from/to\n", scanBudget, t.cursor(q, res.scannedTo))
	}
	if res.total > len(sel) && len(res.tagCount) > 0 {
		which := "all " + totalStr + " matches"
		switch {
		case res.byRange:
			which = fmt.Sprintf("the newest %s matches only", commas(res.tagN))
		case partial && desc:
			which = fmt.Sprintf("the %s counted matches only (newest %s)", commas(res.tagN), spanText(res.firstNs-res.lastNs))
		case partial:
			which = fmt.Sprintf("the %s counted matches only (oldest %s)", commas(res.tagN), spanText(res.lastNs-res.firstNs))
		}
		fmt.Fprintf(&b, "top tags in %s: %s · narrow with e.g. -message:<noise>, min_level=info, or a smaller from/to\n", which, topN(res.tagCount, 5))
	}
	return b.String()
}

// countScope explains a partial count (issue #20): which part of the range
// was counted and how to get an exact count. Empty when the count is exact.
func countScope(res *scanResult, loc *time.Location, desc bool) string {
	if res.countDone || res.lastNs == 0 {
		return ""
	}
	if desc {
		return fmt.Sprintf(" (counted the newest %s only, since %s; older history not counted — narrow with from/to, e.g. from=\"1h\", for an exact count)",
			spanText(res.firstNs-res.lastNs), clock(res.lastNs, loc))
	}
	return fmt.Sprintf(" (counted the oldest %s only, up to %s; newer lines not counted — page with the after cursor, or narrow with from/to for an exact count)",
		spanText(res.lastNs-res.firstNs), clock(res.lastNs, loc))
}

// countFields is the machine-readable count line: total, whether it is
// exact, and for a lower bound where counting stopped (device-local).
func countFields(res *scanResult, loc *time.Location, desc bool) string {
	s := fmt.Sprintf("count: total=%d count_exact=%t", res.total, res.countDone)
	if !res.countDone && res.lastNs != 0 {
		key := "counted_from"
		if !desc {
			key = "counted_to"
		}
		s += fmt.Sprintf(" %s=%q", key, time.Unix(0, res.lastNs).In(loc).Format("2006-01-02 15:04:05.000"))
	}
	return s + "\n"
}

// spanText renders a counted span: "40 s", "21 min", "3 h 5 min".
func spanText(d int64) string {
	t := time.Duration(max(d, 0))
	switch {
	case t < time.Minute:
		return fmt.Sprintf("%d s", int(t.Seconds()))
	case t < time.Hour:
		return fmt.Sprintf("%d min", int(t.Minutes()))
	}
	h := int(t.Hours())
	if m := int(t.Minutes()) - 60*h; m > 0 {
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d h", h)
}

func (t *Tools) rangeNotes(b *strings.Builder, q *query, sel []*model.Record) {
	m := q.d.Store.Meta()
	stats := q.d.Store.Stats()
	if q.fromNs != 0 && stats.OldestNs != 0 && q.fromNs < stats.OldestNs && len(q.d.Store.ClockJumps()) == 0 {
		fmt.Fprintf(b, "note: history starts at device time %s; earlier logs were not recorded (device buffer had rolled over or recording had not started).\n", dateTime(stats.OldestNs, q.loc))
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
			b.WriteString("gap: " + gapText(g, q.loc) + "\n")
		}
	}
}

// zeroHints explains the most likely reasons for an empty result.
func (t *Tools) zeroHints(q *query) string {
	var b strings.Builder
	stats := q.d.Store.Stats()
	if stats.Records == 0 {
		return "hint: nothing recorded for this device yet (recording starts automatically when it is connected).\n"
	}
	if q.toNs != 0 && stats.OldestNs != 0 && q.toNs <= stats.OldestNs {
		fmt.Fprintf(&b, "hint: the device-time window ends before the oldest stored line (device time %s).\n", dateTime(stats.OldestNs, q.loc))
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
		fmt.Fprintf(&b, "hint: no logs from package %q in range.", q.pkg)
		if s := similar(q.pkg, names); len(s) > 0 {
			fmt.Fprintf(&b, " Did you mean %s?", strings.Join(s, ", "))
		}
		if pk := q.d.Packages(); len(pk) > 0 {
			inst := "no"
			for _, p := range allPkgs(pk) {
				if p == q.pkg {
					inst = "yes"
				}
			}
			fmt.Fprintf(&b, " (installed: %s)", inst)
		}
		fmt.Fprintf(&b, " Packages with logs in range: %s\n", topN(pkgs, 8))
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
			fmt.Fprintf(&b, "hint: tag %q has no lines in range (tags are exact and case-sensitive)", tg)
			if len(near) > 0 {
				fmt.Fprintf(&b, "; similar tags: %s", strings.Join(near, ", "))
			}
			b.WriteString("\n")
		}
	}
	if q.filterUsed {
		b.WriteString("hint: check the 'filter parsed as' line above — e.g. same-key terms are ORed, =: is exact and case-sensitive, ~: regexes are case-sensitive.\n")
	}
	if b.Len() == 0 {
		b.WriteString("hint: try a wider from/to, drop min_level, or use view=\"summary\" to see what is there.\n")
	}
	return b.String()
}

func topN(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].v != l[j].v {
			return l[i].v > l[j].v
		}
		return l[i].k < l[j].k
	})
	var parts []string
	for i := 0; i < len(l) && i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s %s", l[i].k, commas(l[i].v)))
	}
	s := strings.Join(parts, " · ")
	if len(l) > n {
		s += fmt.Sprintf(" · …(%d more)", len(l)-n)
	}
	return s
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
	q.d.Store.ScanTime(q.afterSeq, q.beforeSeq, true, q.fromNs, q.toNs, func(r *model.Record) bool {
		n++
		if n&4095 == 0 && time.Since(start) > scanBudget {
			incomplete = true
			return false
		}
		if !q.inTime(r) || !q.pred.Match(r, q.ectx) {
			return true
		}
		total++
		res.addSide(r.Seq)
		// stored order is not time order (clock changes): true min / max
		if newest == 0 || r.TimeNs > newest {
			newest = r.TimeNs
		}
		if oldest == 0 || r.TimeNs < oldest {
			oldest = r.TimeNs
		}
		levels[r.Level.Rank()]++
		tags[r.Tag]++
		procs[procLabel(r)]++
		tmpl[template(r)]++
		if r.Level.Rank() >= 5 && len(errs) < 5 {
			errs = append(errs, r)
		}
		return true
	})
	var b strings.Builder
	rng := ""
	if total > 0 {
		rng = fmt.Sprintf(" · %s–%s", dateTime(oldest, q.loc), dateTime(newest, q.loc))
	}
	fmt.Fprintf(&b, "device %s · device time %s%s · %s\n", q.d.Serial, tzLabel(q.loc, q.d.TZName(), q.clock.Now), rng, clockLine(q.clock))
	b.WriteString("filter parsed as: " + q.interp() + "\n")
	for _, n := range q.notes {
		b.WriteString("note: " + n + "\n")
	}
	t.rangeNotes(&b, q, nil)
	res.countDone = !incomplete
	t.clockNotes(&b, q, res)
	if total == 0 {
		b.WriteString("total 0 lines\n")
		b.WriteString(t.zeroHints(q))
		return b.String()
	}
	fmt.Fprintf(&b, "total %s lines · by level: V %s D %s I %s W %s E %s F %s\n", commas(total),
		commas(levels[1]), commas(levels[2]), commas(levels[3]), commas(levels[4]), commas(levels[5]), commas(levels[6]))
	fmt.Fprintf(&b, "top tags: %s\n", topN(tags, 10))
	fmt.Fprintf(&b, "top processes: %s\n", topN(procs, 5))
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
		b.WriteString("top repeated messages (digits normalized to #):\n")
		for i := 0; i < len(tl) && i < 5; i++ {
			fmt.Fprintf(&b, "  %s× %s\n", commas(tl[i].v), tl[i].k)
		}
	}
	// crashes in range
	var crs, anrs []string
	own := procspkg.NameOwner(q.d.Packages())
	for _, c := range q.d.Crashes() {
		if (q.fromNs != 0 && c.TimeNs < q.fromNs) || (q.toNs != 0 && c.TimeNs >= q.toNs) || c.TimeNs < oldest || c.TimeNs > newest+int64(time.Second) {
			continue
		}
		if q.pkg != "" && c.Package != q.pkg && own(c.Process) != q.pkg {
			continue
		}
		s := fmt.Sprintf("%s %s %s", c.ID, clock(c.TimeNs, q.loc)[:8], shortHeadline(c.Headline))
		if c.Kind == "anr" {
			anrs = append(anrs, s)
		} else {
			crs = append(crs, s)
		}
	}
	fmt.Fprintf(&b, "crashes in range: %d", len(crs))
	if len(crs) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(capList(crs, 3), "; "))
	}
	fmt.Fprintf(&b, " · ANRs: %d", len(anrs))
	if len(anrs) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(capList(anrs, 3), "; "))
	}
	b.WriteString("\n")
	if len(errs) > 0 {
		fmt.Fprintf(&b, "latest errors (%d):\n", len(errs))
		for i := len(errs) - 1; i >= 0; i-- {
			r := errs[i]
			first := strings.SplitN(r.Msg, "\n", 2)[0]
			if len(first) > 200 {
				first = first[:200] + "…"
			}
			fmt.Fprintf(&b, "  %s %s %d %s %s: %s\n", clock(r.TimeNs, q.loc), levelLetter(r.Level), r.PID, procLabel(r), r.Tag, first)
		}
	}
	if incomplete {
		fmt.Fprintf(&b, "note: summary covers only the newest part of the range (scan stopped after %s); narrow from/to for exact numbers\n", scanBudget)
	}
	next := "next: logcat_search with filter/tags/package narrowed (view=\"lines\")"
	if len(crs) > 0 {
		next += fmt.Sprintf(", or logcat_crashes id=%q", strings.Fields(crs[0])[0])
	}
	b.WriteString(next + "\n")
	return b.String()
}

func capList(l []string, n int) []string {
	if len(l) > n {
		return append(l[:n:n], fmt.Sprintf("…%d more", len(l)-n))
	}
	return l
}

func shortHeadline(h string) string {
	h = strings.SplitN(h, "\n", 2)[0]
	if i := strings.Index(h, ": "); i > 0 && strings.Contains(h[:i], ".") {
		h = h[:i]
	}
	if len(h) > 100 {
		h = h[:100] + "…"
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
