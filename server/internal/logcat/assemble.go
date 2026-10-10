package logcat

import (
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Assembler groups physical lines into records: logprint repeats the same
// header for every line of a multi-line message, so consecutive lines with an
// identical header (time, uid, pid, tid, level, tag, buffer) are merged.
type Assembler struct {
	Loc     *time.Location
	Year    int
	buffer  string
	pending *model.Record
	Garbage int // unparseable lines not attached to a record
}

// Buffer returns the current buffer (from the last divider).
func (a *Assembler) Buffer() string { return a.buffer }

// Push feeds one physical line. It returns a completed record, if any.
func (a *Assembler) Push(s string) *model.Record {
	l := ParseLine(s, a.Loc, a.Year)
	if l.Divider != "" {
		out := a.Flush()
		a.buffer = l.Divider
		return out
	}
	if !l.OK {
		if a.pending != nil && s != "" {
			a.pending.Msg += "\n" + s
		} else if s != "" {
			a.Garbage++
		}
		return nil
	}
	if p := a.pending; p != nil && p.TimeNs == l.TimeNs && p.PID == l.PID && p.TID == l.TID &&
		p.Level == l.Level && p.Tag == l.Tag && p.UID == l.UID && p.Buffer == a.buffer {
		p.Msg += "\n" + l.Msg
		return nil
	}
	out := a.Flush()
	a.pending = &model.Record{TimeNs: l.TimeNs, UID: l.UID, PID: l.PID, TID: l.TID, Level: l.Level, Tag: l.Tag, Msg: l.Msg, Buffer: a.buffer}
	return out
}

// Flush returns the pending record (if any) and clears it.
func (a *Assembler) Flush() *model.Record {
	p := a.pending
	a.pending = nil
	return p
}

// HasPending reports whether a record is buffered.
func (a *Assembler) HasPending() bool { return a.pending != nil }
