package mcpserver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

const (
	outputBudget   = 30000 // chars per response
	maxMsgChars    = 2000
	maxStackChars  = 8000
	identicalLabel = "↳"
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

// procLabel names the process of a line. When the pid is unknown (short-lived
// processes such as `log`/`sh`/crash helpers), the uid tells who wrote it:
// "?(uid shell)", "?(uid com.example.app)". The app is never claimed as the
// process without evidence.
func procLabel(r *model.Record) string {
	if r.Process != "" {
		return r.Process
	}
	switch {
	case r.Package != "":
		return "?(uid " + r.Package + ")"
	case r.UID >= 0:
		return "?(uid " + model.UIDName(r.UID) + ")"
	}
	return "?"
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

// formatRecord renders one record in the compact line format.
// indent is prepended to the first line (context lines use two spaces).
func formatRecord(r *model.Record, loc *time.Location, indent string) string {
	msg := r.Msg
	limit := maxMsgChars
	if looksLikeStack(msg) {
		limit = maxStackChars
	}
	if len(msg) > limit {
		cut := limit
		for cut > 0 && !utf8Start(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + fmt.Sprintf("…[+%d chars; narrow with filter pid:%d and from/to around this time to see it whole]", len(r.Msg)-cut, r.PID)
	}
	msg = strings.ReplaceAll(msg, "\n", "\n"+indent+"    ")
	return fmt.Sprintf("%s%s %s %d/%d %s %s: %s", indent, clock(r.TimeNs, loc), levelLetter(r.Level), r.PID, r.TID, procLabel(r), r.Tag, msg)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// lineWriter accumulates output lines, collapsing identical neighbours and
// inserting date separators.
type lineWriter struct {
	loc      *time.Location
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
		w.lines = append(w.lines, fmt.Sprintf("  %s ×%d identical until %s", identicalLabel, w.repeat, clock(w.repeatTo, w.loc)))
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
	w.lines = append(w.lines, formatRecord(r, w.loc, indent))
}

func (w *lineWriter) size() int {
	n := 0
	for _, l := range w.lines {
		n += len(l) + 1
	}
	return n
}

var reDigits = regexp.MustCompile(`\d+`)

// template normalizes digits for "top repeated messages".
func template(r *model.Record) string {
	first := r.Msg
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 160 {
		first = first[:160] + "…"
	}
	return r.Tag + ": " + reDigits.ReplaceAllString(first, "#")
}
