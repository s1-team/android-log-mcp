package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/timeparse"
)

func crashPkgMatch(c *analysis.Crash, pkg string) bool {
	return pkg == "" || c.Package == pkg || pkgOfName(c.Process) == pkg
}

func (t *Tools) crashes(ctx context.Context, a crashArgs) (string, error) {
	if a.Limit == 0 {
		a.Limit = 10
	}
	if a.Limit < 1 || a.Limit > 50 {
		return "", errf("limit must be 1..50 (got %d)", a.Limit)
	}
	kinds := map[string]bool{}
	for _, k := range a.Kinds {
		if k != "java" && k != "native" && k != "anr" {
			return "", errf("kinds: %q is not one of java, native, anr", k)
		}
		kinds[k] = true
	}
	d, err := t.resolveDevice(a.Device)
	if err != nil {
		return "", err
	}
	d.Store.Refresh()
	loc := d.Loc()
	now := d.Now()
	var fromNs, toNs int64
	if a.From != "" {
		tm, err := timeparse.Parse("from", a.From, now)
		if err != nil {
			return "", errf("%v", err)
		}
		fromNs = tm.UnixNano()
	}
	if a.To != "" {
		tm, err := timeparse.Parse("to", a.To, now)
		if err != nil {
			return "", errf("%v", err)
		}
		toNs = tm.UnixNano()
	}
	all := d.Crashes()
	if a.ID != "" {
		for _, c := range all {
			if strings.EqualFold(c.ID, strings.TrimSpace(a.ID)) {
				return t.crashDetail(d, c, all), nil
			}
		}
		var ids []string
		for i, c := range all {
			if i >= 10 {
				break
			}
			ids = append(ids, c.ID)
		}
		if len(ids) == 0 {
			return "", errf("no crash with id %q: no crashes recorded on %s yet", a.ID, d.Serial)
		}
		return "", errf("no crash with id %q on %s. Recent ids: %s (call logcat_crashes without id to list)", a.ID, d.Serial, strings.Join(ids, ", "))
	}
	var sel []*analysis.Crash
	for _, c := range all {
		if !crashPkgMatch(c, a.Package) || (len(kinds) > 0 && !kinds[c.Kind]) {
			continue
		}
		if (fromNs != 0 && c.TimeNs < fromNs) || (toNs != 0 && c.TimeNs >= toNs) {
			continue
		}
		sel = append(sel, c)
	}
	var b strings.Builder
	scope := "all apps"
	if a.Package != "" {
		scope = "package " + a.Package
	}
	if len(sel) == 1 {
		fmt.Fprintf(&b, "device %s · %s · %s · 1 match → full detail\n", d.Serial, tzLabel(loc, d.TZName(), now), scope)
		b.WriteString(t.crashDetail(d, sel[0], all))
		return b.String(), nil
	}
	// group by signature
	type group struct {
		key   string
		items []*analysis.Crash
	}
	var groups []*group
	idx := map[string]*group{}
	for _, c := range sel { // newest first
		sig := c.Signature
		if sig == "" {
			sig = c.Headline
		}
		key := c.Kind + "|" + pkgOfName(c.Process) + "|" + sig
		if c.Kind == "anr" {
			key = c.ID // ANRs are not grouped
		}
		g := idx[key]
		if g == nil {
			g = &group{key: key}
			idx[key] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, c)
	}
	fmt.Fprintf(&b, "device %s · %s · %s · %d crash groups (%d events) in recorded history\n", d.Serial, tzLabel(loc, d.TZName(), now), scope, len(groups), len(sel))
	if st := d.Store.Stats(); st.OldestNs != 0 {
		fmt.Fprintf(&b, "history: logcat from %s; DropBox adds older entries when available (status: %s)\n", dateTime(st.OldestNs, loc), dropboxSummary(d))
	}
	if len(sel) == 0 {
		b.WriteString("no crashes or ANRs match")
		if a.Package != "" {
			b.WriteString(" (package is an exact application id)")
		}
		b.WriteString(".\n")
		return b.String(), nil
	}
	shown := 0
	for _, g := range groups {
		if shown >= a.Limit {
			break
		}
		shown++
		c := g.items[0]
		b.WriteString(crashListEntry(c, loc))
		if len(g.items) > 1 {
			var also []string
			for i, o := range g.items[1:] {
				if i >= 5 {
					also = append(also, fmt.Sprintf("…%d more", len(g.items)-6))
					break
				}
				also = append(also, fmt.Sprintf("%s (%s)", clock(o.TimeNs, loc)[:8], o.ID))
			}
			fmt.Fprintf(&b, "     same signature also at %s\n", strings.Join(also, ", "))
		}
	}
	if len(groups) > shown {
		fmt.Fprintf(&b, "[%d older groups not shown; raise limit or narrow from/to]\n", len(groups)-shown)
	}
	fmt.Fprintf(&b, "more: logcat_crashes id=%q for full detail\n", groups[0].items[0].ID)
	return b.String(), nil
}

func dropboxSummary(d *device.Device) string {
	var parts []string
	for _, tag := range []string{"data_app_crash", "data_app_native_crash", "data_app_anr"} {
		s := d.DropboxStatus(tag)
		if i := strings.IndexByte(s, ' '); i > 0 && strings.HasPrefix(s, "20") {
			s = s[i+1:]
		}
		parts = append(parts, tag+" "+s)
	}
	return strings.Join(parts, ", ")
}

func crashListEntry(c *analysis.Crash, loc *time.Location) string {
	var b strings.Builder
	who := fmt.Sprintf("pid %d %s", c.PID, orQ(c.Process))
	if c.Thread != "" {
		who += " (thread " + c.Thread + ")"
	}
	fmt.Fprintf(&b, "%-4s %s  %-6s %s", c.ID, dateTime(c.TimeNs, loc), c.Kind, who)
	switch c.Kind {
	case "java":
		b.WriteString("\n")
		lines := strings.Split(strings.TrimSpace(firstNonEmpty(c.Body, c.Headline)), "\n")
		fmt.Fprintf(&b, "     %s\n", lines[0])
		frames := 0
		total := 0
		for _, ln := range lines[1:] {
			if strings.HasPrefix(strings.TrimSpace(ln), "at ") {
				total++
				if frames < 3 {
					fmt.Fprintf(&b, "       %s\n", strings.TrimSpace(ln))
					frames++
				}
			}
		}
		if cause := rootCause(lines); cause != "" && cause != lines[0] {
			fmt.Fprintf(&b, "       … root cause: %s\n", cause)
		}
		if total > frames {
			fmt.Fprintf(&b, "       … %d more frames\n", total-frames)
		}
	case "native":
		fmt.Fprintf(&b, "  %s\n", c.Headline)
	case "anr":
		fmt.Fprintf(&b, "  reason: %s\n", orStr(c.Headline, "?"))
		if c.Traces != "" {
			main, _ := analysis.MainThreadTrace(c.Traces)
			if main != "" {
				ml := strings.Split(main, "\n")
				state := ml[0]
				top := ""
				for _, l := range ml[1:] {
					if s := strings.TrimSpace(l); strings.HasPrefix(s, "at ") || strings.HasPrefix(s, "native:") || strings.HasPrefix(s, "- waiting") {
						top = s
						break
					}
				}
				fmt.Fprintf(&b, "     main thread: %s — %s   [traces: %s]\n", state, top, tracesSource(c))
			}
		} else {
			b.WriteString("     main thread: traces not available yet (see detail)\n")
		}
	}
	return b.String()
}

func tracesSource(c *analysis.Crash) string {
	for _, s := range c.Sources {
		if strings.HasPrefix(s, "dropbox:") {
			return strings.TrimPrefix(s, "dropbox:")
		}
	}
	return "dropbox"
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func rootCause(lines []string) string {
	c := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "Caused by: ") {
			c = strings.TrimPrefix(l, "Caused by: ")
		}
	}
	return c
}

// trimStack keeps the first 40 and last 5 frames of each section when the
// whole stack exceeds 20k chars.
func trimStack(s string) string {
	if len(s) <= 20000 {
		return s
	}
	var out []string
	var frames []string
	flush := func() {
		if len(frames) > 45 {
			out = append(out, frames[:40]...)
			out = append(out, fmt.Sprintf("        … %d frames omitted …", len(frames)-45))
			out = append(out, frames[len(frames)-5:]...)
		} else {
			out = append(out, frames...)
		}
		frames = nil
	}
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "at ") || strings.HasPrefix(t, "#") {
			frames = append(frames, l)
			continue
		}
		flush()
		out = append(out, l)
	}
	flush()
	return strings.Join(out, "\n")
}

func (t *Tools) crashDetail(d *device.Device, c *analysis.Crash, all []*analysis.Crash) string {
	loc := d.Loc()
	var b strings.Builder
	kind := map[string]string{"java": "Java crash", "native": "native crash", "anr": "ANR"}[c.Kind]
	who := fmt.Sprintf("pid %d %s", c.PID, orQ(c.Process))
	if c.Thread != "" {
		who += " (thread " + c.Thread + ")"
	}
	fmt.Fprintf(&b, "%s %s · %s · %s · sources: %s\n", c.ID, kind, time.Unix(0, c.TimeNs).In(loc).Format("2006-01-02 15:04:05.000"), who, strings.Join(c.Sources, ", "))
	if c.Package != "" {
		fmt.Fprintf(&b, "package: %s\n", c.Package)
	}
	switch c.Kind {
	case "java":
		b.WriteString("stack:\n")
		b.WriteString(indent(trimStack(firstNonEmpty(c.Body, c.Dropbox, c.Headline)), "  "))
		if c.Dropbox != "" && c.Body != "" && strings.Contains(c.Body, "stack unavailable") {
			b.WriteString("dropbox entry:\n" + indent(trimStack(c.Dropbox), "  "))
		}
	case "native":
		fmt.Fprintf(&b, "signal: %s\n", c.Headline)
		if c.Location != "" {
			fmt.Fprintf(&b, "crash location: %s  (first app frame; libc frames such as abort/syscall come before it)\n", c.Location)
		}
		body := c.Body
		src := "as logged by crash_dump/DEBUG"
		if !strings.Contains(body, "backtrace:") {
			switch {
			case c.Tombstone != "":
				body, src = c.Tombstone, "from DropBox SYSTEM_TOMBSTONE"
			case c.Dropbox != "":
				body, src = c.Dropbox, "from DropBox data_app_native_crash"
			}
		}
		fmt.Fprintf(&b, "tombstone (%s):\n", src)
		b.WriteString(indent(trimStack(capStr(body, 12000)), "  "))
		if c.Tombstone != "" {
			threads := strings.Count(c.Tombstone, "\n--- --- ---") + 1
			fmt.Fprintf(&b, "full tombstone (all %d threads, memory near registers, maps) is in DropBox SYSTEM_TOMBSTONE; `adb shell dumpsys dropbox --print SYSTEM_TOMBSTONE`\n", threads)
		} else if !strings.Contains(body, "backtrace") {
			b.WriteString("note: no tombstone backtrace was logged (DEBUG lines missing) and DropBox had none yet. Fallback: `adb bugreport` (/data/tombstones needs root).\n")
		}
	case "anr":
		fmt.Fprintf(&b, "reason: %s\n", orStr(c.Headline, "?"))
		if c.CPU != "" {
			b.WriteString("cpu (first lines):\n" + indent(c.CPU, "  "))
		}
		if c.Traces != "" {
			main, holder := analysis.MainThreadTrace(c.Traces)
			if main != "" {
				fmt.Fprintf(&b, "main thread [traces: %s]:\n", tracesSource(c))
				b.WriteString(indent(capStr(main, 8000), "  "))
				if holder != "" {
					b.WriteString("lock holder thread:\n" + indent(capStr(holder, 6000), "  "))
				}
			} else {
				b.WriteString("traces present but no \"main\" thread block found; first part:\n" + indent(capStr(c.Traces, 4000), "  "))
			}
		} else {
			fmt.Fprintf(&b, "traces: unavailable — no matching dropbox data_app_anr entry (dropbox status: %s). It may be rate-limited, not written yet (retry in a few seconds), or dumpsys dropbox not permitted. /data/anr needs root. Fallback: run \"adb bugreport\" yourself.\n", d.DropboxStatus("data_app_anr"))
		}
		if c.Body != "" && !strings.Contains(c.Body, "CPU usage") {
			b.WriteString("ActivityManager block:\n" + indent(capStr(c.Body, 3000), "  "))
		}
	}
	// same-signature history
	var same []string
	for _, o := range all {
		if o.ID != c.ID && o.Kind == c.Kind && o.Signature != "" && o.Signature == c.Signature {
			same = append(same, fmt.Sprintf("%s (%s)", o.ID, dateTime(o.TimeNs, loc)))
		}
	}
	if len(same) > 0 {
		fmt.Fprintf(&b, "same signature: %s\n", strings.Join(capList(same, 8), ", "))
	}
	// last 40 lines of the crashed process before the crash
	if c.Seq == 0 || c.PID == 0 {
		b.WriteString("before crash: not in recorded logs (known only from DropBox — happened before recording started or the log buffer rolled over)\n")
		return b.String()
	}
	var before []*model.Record
	scanned := 0
	d.Store.Scan(0, c.Seq, true, func(r *model.Record) bool {
		scanned++
		if r.Boot != c.Boot && c.Boot != "" {
			return false
		}
		if r.PID == c.PID {
			before = append(before, r)
		}
		return len(before) < 40 && scanned < 300000
	})
	fmt.Fprintf(&b, "before crash (pid %d, last %d lines):\n", c.PID, len(before))
	w := &lineWriter{loc: loc}
	for i := len(before) - 1; i >= 0; i-- {
		w.add(before[i], false)
	}
	w.flushRepeat()
	for _, l := range w.lines {
		b.WriteString("  " + l + "\n")
	}
	out := b.String()
	if len(out) > outputBudget {
		out = out[:outputBudget] + "\n…[truncated at 30k chars]\n"
	}
	return out
}

func indent(s, p string) string {
	s = strings.TrimRight(s, "\n")
	return p + strings.ReplaceAll(s, "\n", "\n"+p) + "\n"
}

func capStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…[+%d chars]", len(s)-n)
}

var _ = sort.Strings
