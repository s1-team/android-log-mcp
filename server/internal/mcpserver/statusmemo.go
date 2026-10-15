package mcpserver

import (
	"context"
	"strings"
	"sync"
)

// StatusLine is one line at the start of logcat_status (the service
// process's own lines, adb, store). Static lines do not change from call to
// call: logcat_status shows them on the first call of a connection, again
// when one of them changed, and always with verbose=true (#23).
type StatusLine struct {
	Text   string
	Static bool
	Label  string // short name for the "unchanged" line, e.g. "linger"
	Key    string // what counts as a change (default: Text)
}

// StatusMemo remembers, for one connection to the service process (one MCP
// or CLI process), the static lines that logcat_status last showed.
type StatusMemo struct {
	mu    sync.Mutex
	shown string
	ok    bool
}

// changed records sig and reports whether it differs from the last one.
func (m *StatusMemo) changed(sig string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ok && m.shown == sig {
		return false
	}
	m.shown, m.ok = sig, true
	return true
}

type statusMemoKey struct{}

// WithStatusMemo attaches the connection's memo to a tool call. Without one
// (in-process --offline use, tests), logcat_status always shows every line.
func WithStatusMemo(ctx context.Context, m *StatusMemo) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, statusMemoKey{}, m)
}

func statusMemoFrom(ctx context.Context) *StatusMemo {
	m, _ := ctx.Value(statusMemoKey{}).(*StatusMemo)
	return m
}

// renderStatusHead writes lines; the static ones collapse into one line when
// they are unchanged since this connection last showed them.
func renderStatusHead(b *strings.Builder, lines []StatusLine, memo *StatusMemo, verbose bool) {
	var keys, labels []string
	for _, l := range lines {
		if l.Static {
			k := l.Key
			if k == "" {
				k = l.Text
			}
			keys = append(keys, k)
			labels = append(labels, l.Label)
		}
	}
	show := len(keys) == 0 || memo == nil
	if !show {
		show = memo.changed(strings.Join(keys, "\n")) || verbose
	}
	placed := false
	for _, l := range lines {
		if l.Static && !show {
			if !placed {
				b.WriteString(hdr("unchanged", "since", "earlier_in_session", "lines", strings.Join(noSpaces(labels), ",")) + nextClause("verbose=true") + "\n")
				placed = true
			}
			continue
		}
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
}

func noSpaces(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = noSpace(s)
	}
	return out
}
