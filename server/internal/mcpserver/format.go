package mcpserver

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

const (
	outputBudget    = 30000 // chars per response
	maxMsgChars     = 2000  // per line
	maxStackChars   = 8000  // per line holding a stack, stack="full"
	shortStackChars = 2000  // per line holding a stack, stack="short" (#23)
	identicalLabel  = "^"
	// fmtVersion is the output-format version printed as fmt=N in every
	// response header (#85); one bump per breaking change of the format.
	fmtVersion = 1
)

func tzLabel(loc *time.Location, name string, at time.Time) string {
	_, off := at.In(loc).Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s (%s%02d:%02d)", name, sign, off/3600, off%3600/60)
}

func commas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func clock(ns int64, loc *time.Location) string {
	return time.Unix(0, ns).In(loc).Format("15:04:05.000")
}

func dateTime(ns int64, loc *time.Location) string {
	return time.Unix(0, ns).In(loc).Format("2006-01-02 15:04:05")
}

func dateOf(ns int64, loc *time.Location) string {
	return time.Unix(0, ns).In(loc).Format("2006-01-02")
}

// procLabel names the process of a line as ONE token (no spaces; #85 P5).
// When the pid is unknown (short-lived processes such as `log`/`sh`/crash
// helpers), the uid tells who wrote it: "uid:shell", "uid:com.example.app".
// The app is never claimed as the process without evidence; "?" = unknown.
func procLabel(r *model.Record) string {
	if r.Process != "" {
		return noSpace(r.Process)
	}
	switch {
	case r.Package != "":
		return "uid:" + noSpace(r.Package)
	case r.UID >= 0:
		return "uid:" + noSpace(model.UIDName(r.UID))
	}
	return "?"
}

func noSpace(s string) string {
	if strings.ContainsAny(s, " \t") {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' {
				return '_'
			}
			return r
		}, s)
	}
	return s
}

func levelLetter(l model.Level) string {
	if l == 0 {
		return "?"
	}
	return string(rune(l))
}

func looksLikeStack(msg string) bool {
	return strings.Contains(msg, "\n\tat ") || strings.Contains(msg, "\n    at ") || strings.Contains(msg, "\n  at ")
}

// formatRecord renders one record in the compact line format, with the
// whole message (stack="full"). indent is prepended to the first line
// (context lines use two spaces).
func formatRecord(r *model.Record, loc *time.Location, indent string) string {
	return formatLine(r, loc, indent, capMsg(r.Msg))
}

// capMsg applies the per-line cap. The cap is the same for every query, so
// the marker says that narrowing does not help.
func capMsg(msg string) string {
	limit := maxMsgChars
	if looksLikeStack(msg) {
		limit = maxStackChars
	}
	if len(msg) <= limit {
		return msg
	}
	cut := cutAt(msg, limit)
	return msg[:cut] + lineCut(limit, len(msg)-cut, "none")
}

// cutAt is the largest UTF-8 boundary <= n.
func cutAt(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return n
}

func formatLine(r *model.Record, loc *time.Location, indent, msg string) string {
	msg = strings.ReplaceAll(msg, "\n", "\n"+indent+"| ")
	return fmt.Sprintf("%s%s %s %d/%d %s %s: %s", indent, clock(r.TimeNs, loc), levelLetter(r.Level), r.PID, r.TID, procLabel(r), r.Tag, msg)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// lineWriter accumulates output lines, collapsing identical neighbours and
// inserting date separators.
type lineWriter struct {
	loc *time.Location
	// short: stacks as in stack="short" (#23); seen (non-nil to enable)
	// maps the frames of each stack printed so far to its time
	short    bool
	seen     map[string]int64
	stats    stackStats
	cache    map[*model.Record]shortRes // shortStack results, shared by the writers of one response
	lines    []string
	lastDate string
	prev     *model.Record
	repeat   int
	repeatTo int64
}

func sameContent(a, b *model.Record) bool {
	return a != nil && b != nil && a.PID == b.PID && a.Tag == b.Tag && a.Level == b.Level && a.Msg == b.Msg
}

func (w *lineWriter) flushRepeat() {
	if w.repeat > 0 {
		w.lines = append(w.lines, fmt.Sprintf("%s rep=%d until=%s", identicalLabel, w.repeat, clock(w.repeatTo, w.loc)))
		w.repeat = 0
	}
}

// add appends r; ctx marks a context line.
func (w *lineWriter) add(r *model.Record, ctx bool) {
	if !ctx && sameContent(w.prev, r) {
		w.repeat++
		w.repeatTo = r.TimeNs
		return
	}
	w.flushRepeat()
	if d := dateOf(r.TimeNs, w.loc); d != w.lastDate {
		if w.lastDate != "" {
			w.lines = append(w.lines, "--- "+d+" ---")
		}
		w.lastDate = d
	}
	indent := ""
	if ctx {
		indent = "  "
		w.prev = nil
	} else {
		w.prev = r
	}
	w.lines = append(w.lines, w.format(r, indent))
}

type shortRes struct {
	msg     string
	hidden  int
	changed bool
}

// format renders r; with short, a stack is shortened (shortStack) or, if
// the same frames were already printed in this response, replaced by a
// reference to them.
func (w *lineWriter) format(r *model.Record, indent string) string {
	if !w.short || !looksLikeStack(r.Msg) {
		return formatRecord(r, w.loc, indent)
	}
	key := stackKey(r.Msg)
	if at, ok := w.seen[key]; ok && key != "" {
		msg, hidden := sameStack(r.Msg, at, w.loc)
		w.stats.shortened++
		w.stats.hidden += hidden
		return formatLine(r, w.loc, indent, msg)
	}
	res, ok := w.cache[r]
	if !ok {
		res.msg, res.hidden, res.changed = shortStack(r.Msg)
		if w.cache != nil {
			w.cache[r] = res
		}
	}
	msg, hidden := res.msg, res.hidden
	if res.changed {
		w.stats.shortened++
		w.stats.hidden += hidden
	}
	if w.seen != nil && key != "" {
		w.seen[key] = r.TimeNs
	}
	return formatLine(r, w.loc, indent, msg)
}

func (w *lineWriter) size() int {
	n := 0
	for _, l := range w.lines {
		n += len(l) + 1
	}
	return n
}

// template normalizes digits for "top repeated messages".
func template(r *model.Record) string {
	first := r.Msg
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 160 {
		first = first[:cutAt(first, 160)] + "..."
	}
	return r.Tag + ": " + collapseDigits(first)
}

// ---- shared response grammar (#85, docs/GOALS.md "Agent-first") ----------
//
// Every tool response starts with one header line "ok|empty|error k=v ...".
// Truncation, empty and error lines share one shape: "<word> k=v ... next:
// param=value [| param=value]". Plain functions over key=value pairs; no
// state, so tests assert the text of the tool response, not these helpers.

// val formats a header value: integers, floats (1 decimal) and bools (1/0)
// plain, strings quoted only when they contain a space, quote or are empty.
func val(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" || strings.ContainsAny(x, " \t\"=") {
			return strconv.Quote(x)
		}
		return x
	case bool:
		if x {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(x, 'f', 1, 64)
	}
	return fmt.Sprint(v)
}

// hdr builds "word k=v k=v" from alternating keys and values.
func hdr(word string, kv ...any) string {
	var b strings.Builder
	b.WriteString(word)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(" " + kv[i].(string) + "=" + val(kv[i+1]))
	}
	return b.String()
}

// param formats one executable parameter for a next: clause: strings are
// always quoted (copy-paste safe), numbers and bools plain.
func param(k string, v any) string {
	if s, ok := v.(string); ok {
		return k + "=" + strconv.Quote(s)
	}
	return k + "=" + val(v)
}

// nextClause is " next: a=b | c=d" (empty without alternatives).
func nextClause(alts ...string) string {
	if len(alts) == 0 {
		return ""
	}
	return " next: " + strings.Join(alts, " | ")
}

// truncLine is the one-line truncation marker: "truncated what=N exact=1
// [k=v ...] next: ...". exact=0 replaces the old ">=" glyph (a lower bound).
func truncLine(what string, n int, exact bool, extra []any, alts ...string) string {
	kv := append([]any{what, n, "exact", exact}, extra...)
	return hdr("truncated", kv...) + nextClause(alts...) + "\n"
}

// lineCut marks a per-line cut inside a row, at the place it was cut. next
// is "none" when no parameter shows more.
func lineCut(limit, more int, next string) string {
	return fmt.Sprintf("...[line_cut=%d more_chars=%d next: %s]", limit, more, next)
}

// isoNs is an ISO-8601 time with offset and milliseconds in loc.
func isoNs(ns int64, loc *time.Location) string {
	return time.Unix(0, ns).In(loc).Format("2006-01-02T15:04:05.000Z07:00")
}

// isoSec is an ISO-8601 time with offset, seconds precision.
func isoSec(t time.Time) string { return t.Format("2006-01-02T15:04:05Z07:00") }

// secs is a signed duration in seconds with one decimal (key suffix _s).
func secs(d time.Duration) float64 { return d.Seconds() }

// gapLine is a recording gap overlapping the result.
func gapLine(g store.Gap, loc *time.Location) string {
	to, from := "ongoing", "?"
	if g.ToNs > 0 {
		to = isoNs(g.ToNs, loc)
	}
	if g.FromNs > 0 {
		from = isoNs(g.FromNs, loc)
	}
	return hdr("gap", "from", from, "to", to, "reason", g.Reason)
}

// collapseDigits replaces every run of ASCII digits by one "#" (the same as
// \d+ -> "#", without a regexp: it runs for thousands of hits per call).
func collapseDigits(s string) string {
	i := 0
	for i < len(s) && (s[i] < '0' || s[i] > '9') {
		i++
	}
	if i == len(s) {
		return s
	}
	b := make([]byte, 0, len(s))
	b = append(b, s[:i]...)
	for i < len(s) {
		if c := s[i]; c >= '0' && c <= '9' {
			b = append(b, '#')
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			continue
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}
