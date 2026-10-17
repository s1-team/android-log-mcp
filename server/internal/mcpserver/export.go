package mcpserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/s1crab/android-log-mcp/server/internal/filter"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/redact"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// logcat_export (issue #172): writes the matching lines of a window to a
// text file under <root>/export/ in `logcat -v threadtime -v year` form and
// answers with the file's path and counts, never with log lines. The tool
// has no output path (its arguments come from a model that reads untrusted
// log text), and writes are atomic (temp file + rename, layout.AtomicFile).

const (
	exportDefaultMB = 64
	exportMaxMB     = 512
	exportMaxExtra  = 8
	exportListMax   = 20 // clock jumps / gaps listed in the header
)

// Variables for tests.
var (
	// exportBudget is the time the scan may run; a longer export stops there
	// with truncated ... cap=time_budget and a cursor to continue.
	exportBudget = 20 * time.Second
	exportSince  = time.Since
	exportNow    = time.Now
	// exportWrap wraps the file writer (ENOSPC injection).
	exportWrap func(io.Writer) io.Writer
)

type exportArgs struct {
	Device      string   `json:"device"`
	Filter      string   `json:"filter"`
	Package     string   `json:"package"`
	Tags        []string `json:"tags"`
	MinLevel    string   `json:"min_level"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Before      string   `json:"before"`
	After       string   `json:"after"`
	Redact      bool     `json:"redact"`
	RedactExtra []string `json:"redact_extra"`
	MaxMB       int      `json:"max_mb"`
	Verbose     bool     `json:"verbose"`
}

const unredactedWarning = "not redacted. Lines are exactly as recorded and may contain tokens, passwords or personal data. Re-export with redact=true (CLI: --redact) before sharing."

func (t *Tools) export(ctx context.Context, a exportArgs) (out string, err error) {
	if a.MaxMB == 0 {
		a.MaxMB = exportDefaultMB
	}
	if a.MaxMB < 1 || a.MaxMB > exportMaxMB {
		return "", errc("bad_param", []string{param("max_mb", exportDefaultMB)}, "param", "max_mb", "value", a.MaxMB, "valid", "1.."+strconv.Itoa(exportMaxMB))
	}
	if len(a.RedactExtra) > 0 && !a.Redact {
		return "", errc("bad_param", []string{"redact=true"}, "param", "redact_extra", "msg", "only with redact=true")
	}
	if len(a.RedactExtra) > exportMaxExtra {
		return "", errc("bad_param", []string{"redact_extra=[first 8 patterns]"}, "param", "redact_extra", "value", len(a.RedactExtra), "valid", "max "+strconv.Itoa(exportMaxExtra))
	}
	red, rerr := redact.New(redact.Options{Disabled: !a.Redact, Extra: a.RedactExtra, Home: userHome()})
	if rerr != nil {
		return "", errc("bad_param", []string{"redact_extra=[]"}, "param", "redact_extra", "msg", rerr.Error())
	}
	if t.ExportRoot.Dir == "" {
		return "", errc("export_failed", nil, "msg", "no file can be written with --offline")
	}
	d, done, note, err := t.useAndWait(ctx, a.Device)
	defer done()
	if err != nil {
		return "", err
	}
	defer func() {
		if note != "" && err == nil {
			head, rest, _ := strings.Cut(out, "\n")
			out = head + "\n" + note + "\n" + rest
		}
	}()
	d.Store.Refresh()
	q, err := t.buildQuery(d, commonArgs{verbose: a.Verbose, filter: a.Filter, pkg: a.Package, minLevel: a.MinLevel, from: a.From, to: a.To, before: a.Before, after: a.After, tags: a.Tags})
	if err != nil {
		return "", err
	}
	capBytes := int64(a.MaxMB) << 20
	dir := t.ExportRoot.Export()
	if _, serr := os.Stat(dir); serr != nil {
		if err := t.ExportRoot.EnsureExport(); err != nil {
			return "", errc("export_failed", []string{"android-log-mcp service status"}, "msg", err.Error())
		}
	}
	if g := t.M.Disk; g != nil && g.MinFree > 0 {
		fn := g.Free
		if fn == nil {
			fn = store.FreeSpace
		}
		if free, ferr := fn(dir); ferr == nil && free < uint64(capBytes)+g.MinFree {
			kv := []any{"free_mb", free >> 20, "need_mb", (uint64(capBytes) + g.MinFree) >> 20}
			var next []string
			if free > g.MinFree+(1<<20) {
				next = []string{param("max_mb", min(int((free-g.MinFree)>>20), a.MaxMB-1))}
				if a.MaxMB == 1 {
					next = nil
				}
			}
			return "", errc("disk_low", next, kv...)
		}
	}
	af, err := layout.NewAtomicFile(dir)
	if err != nil {
		return "", errc("export_failed", []string{"android-log-mcp service status"}, "msg", err.Error())
	}
	defer af.Abort() // no-op after a successful Commit

	var sink io.Writer = af
	if exportWrap != nil {
		sink = exportWrap(af)
	}
	cw := &countWriter{w: sink}
	bw := bufio.NewWriterSize(cw, 64<<10)
	st := d.Store
	upper := q.beforeSeq
	if upper == 0 {
		upper = st.LastSeq() + 1
	}
	jumps := q.jumpsIn(upper).jumps
	hostNow := exportNow()
	t.exportHeader(bw, q, a, red, jumps, upper, hostNow)

	start := exportNow()
	var (
		matched, lines  int
		lastSeq         = q.afterSeq
		ji              int
		firstNs, lastNs int64
		stop            string // "", "max_mb", "time_budget"
		werr, cerr      error
		chunk           []byte
		prefix          []byte
		scanned         int
	)
	visit := func(r *model.Record) bool {
		scanned++
		if scanned&255 == 0 {
			if e := ctx.Err(); e != nil {
				cerr = e
				return false
			}
			if exportSince(start) > exportBudget {
				stop = "time_budget"
				return false
			}
		}
		if !q.inTime(r) || !q.pred.Match(r, q.ectx) {
			return true
		}
		chunk = chunk[:0]
		k := ji
		for k < len(jumps) && jumps[k].Seq <= r.Seq {
			chunk = append(chunk, "--------- clock jump "+jumpKV(jumps[k], q.loc)+"\n"...)
			k++
		}
		msg := r.Msg
		if a.Redact {
			msg = red.Text(msg)
		}
		var n int
		chunk, prefix, n = appendLogLines(chunk, prefix, r, q.loc, msg)
		if matched > 0 && cw.n+int64(bw.Buffered())+int64(len(chunk)) > capBytes {
			stop = "max_mb"
			return false
		}
		if _, e := bw.Write(chunk); e != nil {
			werr = e
			return false
		}
		ji = k
		matched++
		lines += n
		lastSeq = r.Seq
		if firstNs == 0 || r.TimeNs < firstNs {
			firstNs = r.TimeNs
		}
		lastNs = max(lastNs, r.TimeNs)
		return true
	}
	countable := !q.countByRange() && filter.Countable(q.pred)
	var classIDs map[uint32]bool
	if countable {
		st.EnsureIndex()
		classIDs = st.MatchingClassIDs(func(r *model.Record) bool { return q.pred.Match(r, q.ectx) })
		st.ScanTimeClass(q.afterSeq, upper, false, q.fromNs, q.toNs, classIDs, visit)
	} else {
		st.ScanTime(q.afterSeq, upper, false, q.fromNs, q.toNs, visit)
	}
	if cerr != nil {
		return "", errf("canceled: %v", cerr)
	}
	if werr != nil {
		return "", exportWriteErr(werr)
	}
	if matched == 0 && stop == "" {
		af.Abort()
		scannedN, hints := t.zeroHints(q)
		extra := []any{"matched", 0, "scanned", scannedN}
		if s := st.Stats(); s.Records > 0 {
			extra = append(extra, "range", isoNs(s.OldestNs, q.loc)+"/"+isoNs(s.NewestNs, q.loc))
		}
		var b strings.Builder
		b.WriteString(t.head(q, "empty", "export", extra...))
		if q.filterUsed || q.verbose {
			b.WriteString(hdr("filter", "parsed", q.interp()) + "\n")
		}
		for _, n := range q.notes {
			b.WriteString(n + "\n")
		}
		b.WriteString(hints)
		return b.String(), nil
	}

	// what is left after a stop: counted exactly when the index allows
	rest, restExact := 0, true
	if stop != "" {
		match := func(r *model.Record) bool { return q.pred.Match(r, q.ectx) }
		switch {
		case q.countByRange():
			rest = int(st.Count(lastSeq, upper))
		case countable:
			rest = int(st.CountMatch(lastSeq, upper, q.fromNs, q.toNs, classIDs, match))
		default:
			cstart := time.Now()
			nn := 0
			st.ScanTime(lastSeq, upper, false, q.fromNs, q.toNs, func(r *model.Record) bool {
				nn++
				if nn&4095 == 0 && time.Since(cstart) > countBudget {
					restExact = false
					return false
				}
				if q.inTime(r) && q.pred.Match(r, q.ectx) {
					rest++
				}
				return true
			})
		}
	}
	hits := 0
	for _, n := range red.Stats() {
		hits += n
	}
	lastCursor := t.cursor(q, lastSeq)
	trunc := "0"
	if stop != "" {
		trunc = stop
	}
	// the footer states the size of the whole file, itself included
	var footer string
	for i, size := 0, cw.n+int64(bw.Buffered()); i < 4; i++ {
		footer = "# " + hdr("end", "lines", lines, "bytes", size, "matched", matched+rest, "exact", restExact, "truncated", trunc, "redaction_hits", hits, "last_cursor", lastCursor) + "\n"
		if n := cw.n + int64(bw.Buffered()) + int64(len(footer)); n == size {
			break
		} else {
			size = n
		}
	}
	bw.WriteString(footer)
	if err := bw.Flush(); err != nil {
		return "", exportWriteErr(err)
	}
	size := cw.n
	label := q.d.Serial
	if a.Redact {
		sum := sha256.Sum256([]byte(label))
		label = "dev-" + hex.EncodeToString(sum[:4])
	}
	final, err := layout.FreeName(dir, func(n int) string { return layout.ExportName(label, hostNow, n) })
	if err == nil {
		err = af.Commit(final, false)
	}
	if err != nil {
		return "", errc("export_failed", []string{"android-log-mcp service status"}, "msg", err.Error())
	}

	extra := []any{"file", final, "bytes", size, "lines", lines, "matched", matched + rest, "exact", restExact,
		"range", isoNs(firstNs, q.loc) + "/" + isoNs(lastNs, q.loc), "order", "arrival",
		"redacted", a.Redact}
	if a.Redact {
		extra = append(extra, "redaction_hits", hits)
	}
	extra = append(extra, "clock_jumps", len(jumps), "expires", isoSec(hostNow.Add(layout.ExportMaxAge)))
	var b strings.Builder
	b.WriteString(t.head(q, "ok", "export", extra...))
	for _, n := range q.notes {
		b.WriteString(n + "\n")
	}
	if !a.Redact {
		b.WriteString(hdr("note", "code", "unredacted", "msg", "lines are written as recorded and may contain tokens or personal data; pass redact=true to mask them") + nextClause("redact=true") + "\n")
	}
	if stop != "" {
		var why string
		kv := []any{"cap", stop}
		var alts []string
		switch stop {
		case "time_budget":
			why = fmt.Sprintf("the file is INCOMPLETE: the export stopped after the %d s time budget (large redacted exports are slow)", int(exportBudget.Seconds()))
			kv = append(kv, "scan_stopped_ms", int(exportBudget.Milliseconds()))
		default:
			why = fmt.Sprintf("the file is INCOMPLETE: it reached max_mb=%d", a.MaxMB)
			if a.MaxMB < exportMaxMB {
				alts = []string{param("max_mb", min(a.MaxMB*2, exportMaxMB))}
			}
		}
		b.WriteString(hdr("note", "code", "truncated", "msg", why+"; the rest follows from the cursor") + "\n")
		b.WriteString(truncLine("newer", rest, restExact, kv, append([]string{param("after", lastCursor)}, alts...)...))
	}
	return b.String(), nil
}

func exportWriteErr(err error) error {
	return errc("export_failed", []string{"android-log-mcp service status"}, "msg", err.Error())
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// countWriter counts the bytes that reached the file.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// jumpKV is the key=value part of a clock jump marker or header line.
func jumpKV(j store.ClockJump, loc *time.Location) string {
	host := "-"
	if j.HostNs != 0 {
		host = time.Unix(0, j.HostNs).Local().Format(time.RFC3339)
	}
	return kvLine("seq", j.Seq, "source", j.Source, "delta_s", secs(time.Duration(j.DeltaNs)),
		"device_before", isoOrDash(j.BeforeNs, loc), "device_after", isoOrDash(j.AfterNs, loc), "host", host)
}

// kvLine is "k=v k=v" with the quoting rule of hdr.
func kvLine(kv ...any) string { return strings.TrimPrefix(hdr("x", kv...), "x ") }

// exportHeader writes the "# " header, from what is known before the scan.
// The body never starts with '#': every body line starts with a year digit
// and every marker with "--------- ".
func (t *Tools) exportHeader(w *bufio.Writer, q *query, a exportArgs, red *redact.Redactor, jumps []store.ClockJump, upper uint64, hostNow time.Time) {
	m := q.d.Store.Meta()
	line := func(s string) { w.WriteString("# " + s + "\n") }
	// text that can carry a secret (a filter value, a gap reason) goes through the redactor
	txt := func(s string) string {
		if a.Redact {
			return red.Text(s)
		}
		return s
	}
	line("android-log-mcp export " + kvLine("fmt", fmtVersion, "version", Version))
	boot := "-"
	if n := len(m.Sessions); n > 0 {
		boot = m.Sessions[n-1].Boot
	}
	line(kvLine("device", q.d.Serial, "model", m.Model, "api", m.SDK, "release", m.Release, "boot", boot, "tz", q.d.TZName()))
	ck := strings.Fields(kvLine(clockKV(q.clock, false)...))
	line(kvLine("exported_at", isoSec(hostNow)) + " (host time) " + strings.Join(ck, " "))
	after, before := "-", "-"
	if a.After != "" {
		after = a.After
	}
	if a.Before != "" {
		before = a.Before
	}
	line("window " + kvLine("from", isoOrDash(q.fromNs, q.loc), "to", isoOrDash(q.toNs, q.loc)) + " (device time) " + kvLine("after", after, "before", before))
	line("filter " + kvLine("parsed", txt(q.interp())))
	if a.Redact {
		line(kvLine("redacted", true, "rules", "builtin", "extra_rules", len(a.RedactExtra)))
		line("note: pattern-based redaction, not complete; tag, process, package, pid and device properties are not redacted; numbers in [REDACTED:<type>#n] are valid within this file only (random key)")
	} else {
		line(kvLine("redacted", false))
		line("WARNING: " + unredactedWarning)
	}
	line(kvLine("clock_jumps", len(jumps)))
	for i, j := range jumps {
		if i == exportListMax {
			line(kvLine("clock_jumps_more", len(jumps)-exportListMax))
			break
		}
		line("clock_jump " + jumpKV(j, q.loc))
	}
	lo, hi := q.fromNs, q.toNs
	ng := 0
	for _, g := range m.Gaps {
		gto := g.ToNs
		if gto <= 0 {
			gto = 1 << 62
		}
		if (hi != 0 && g.FromNs >= hi) || gto <= lo {
			continue
		}
		if ng == exportListMax {
			line(kvLine("gaps_more", "yes"))
			break
		}
		ng++
		line("gap " + kvLine("from", isoOrDash(g.FromNs, q.loc), "to", isoOrDash(g.ToNs, q.loc), "reason", txt(g.Reason)))
	}
	var ids []string
	cr, an := 0, 0
	for _, c := range q.d.Crashes() {
		if (q.fromNs != 0 && c.TimeNs < q.fromNs) || (q.toNs != 0 && c.TimeNs >= q.toNs) {
			continue
		}
		if c.Kind == "anr" {
			an++
		} else {
			cr++
		}
		if len(ids) < exportListMax {
			ids = append(ids, c.ID)
		}
	}
	line(kvLine("crashes", cr, "anrs", an, "ids", strings.Join(ids, ",")))
	line("caps " + kvLine("max_mb", a.MaxMB, "time_budget_s", int(exportBudget.Seconds())))
	line("---")
}

// appendLogLines appends r as `logcat -v threadtime -v year` lines (device
// time in loc): "2026-10-17 09:12:03.456  1234  1250 E OkHttp  : message".
// A multi-line message repeats the header on every line, as logcat prints it.
// prefix is a scratch buffer; the number of lines is returned.
func appendLogLines(b, prefix []byte, r *model.Record, loc *time.Location, msg string) ([]byte, []byte, int) {
	prefix = time.Unix(0, r.TimeNs).In(loc).AppendFormat(prefix[:0], "2006-01-02 15:04:05.000")
	prefix = append(prefix, ' ')
	prefix = appendPad(prefix, r.PID, 5)
	prefix = append(prefix, ' ')
	prefix = appendPad(prefix, r.TID, 5)
	lv := byte(r.Level)
	if lv == 0 {
		lv = 'I'
	}
	prefix = append(prefix, ' ', lv, ' ')
	prefix = append(prefix, r.Tag...)
	for n := utf8.RuneCountInString(r.Tag); n < 8; n++ {
		prefix = append(prefix, ' ')
	}
	prefix = append(prefix, ':', ' ')
	n := 0
	for {
		ln, rest, more := strings.Cut(msg, "\n")
		b = append(b, prefix...)
		b = append(b, strings.TrimRight(ln, "\r")...)
		b = append(b, '\n')
		n++
		if !more {
			break
		}
		msg = rest
	}
	return b, prefix, n
}

func appendPad(b []byte, v, width int) []byte {
	s := strconv.Itoa(v)
	for i := len(s); i < width; i++ {
		b = append(b, ' ')
	}
	return append(b, s...)
}
