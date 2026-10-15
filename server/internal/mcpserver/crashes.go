package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// crashPkgMatch: own maps a process name to its package (procs.NameOwner),
// so crashes in dotted processes (com.google.android.gms.persistent) and
// crashes stored with the process name as package match (issue #41). su
// (nil unless pkg is on a shared uid) adds crashes of the uid's shared
// processes (issue #44); a stored "android" counts only for system_server
// (issue #70).
func crashPkgMatch(c *analysis.Crash, pkg string, own func(string) string, su *sharedUID) bool {
	return pkg == "" || crashPackage(c.Package, c.Process) == pkg || own(c.Process) == pkg || su.process(c.Process)
}

// beforeLinesDefault / beforeLinesMax: how many lines of the crashed process
// before the crash a detail shows (param before_lines; was a fixed 40).
const (
	beforeLinesDefault = 20
	beforeLinesMax     = 100
)

// crashHead is the first line of a logcat_crashes response.
func crashHead(d *device.Device, ci device.ClockInfo, verbose bool, word, view string, kv ...any) string {
	all := append([]any{"view", view, "device", d.Serial}, kv...)
	if verbose {
		all = append(all, "tz", d.TZName())
	}
	all = append(all, clockKV(ci, verbose)...)
	all = append(all, "fmt", fmtVersion)
	return hdr(word, all...) + "\n"
}

func (t *Tools) crashes(ctx context.Context, a crashArgs) (out string, err error) {
	if a.Limit == 0 {
		a.Limit = 10
	}
	if a.Limit < 1 || a.Limit > 50 {
		return "", errc("bad_param", []string{"limit=50"}, "param", "limit", "value", a.Limit, "valid", "1..50")
	}
	if a.BeforeLines == 0 {
		a.BeforeLines = beforeLinesDefault
	}
	if a.BeforeLines < 1 || a.BeforeLines > beforeLinesMax {
		return "", errc("bad_param", []string{param("before_lines", beforeLinesMax)}, "param", "before_lines", "value", a.BeforeLines, "valid", "1.."+strconv.Itoa(beforeLinesMax))
	}
	kinds := map[string]bool{}
	for _, k := range a.Kinds {
		if k != "java" && k != "native" && k != "anr" {
			return "", errc("bad_param", []string{`kinds=["java","native","anr"]`}, "param", "kinds", "value", k, "valid", "java,native,anr")
		}
		kinds[k] = true
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
	loc := d.Loc()
	ci, fromNs, toNs, err := window(d, a.From, a.To, 0)
	if err != nil {
		return "", err
	}
	all := d.Crashes()
	if a.ID != "" {
		for _, c := range all {
			if strings.EqualFold(c.ID, strings.TrimSpace(a.ID)) {
				if full := d.CrashFull(c.ID)[0]; full != nil {
					c = full
				}
				return t.crashDetail(d, ci, c, all, a), nil
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
			return "", errc("unknown_crash", []string{`logcat_search filter="is:crash"`}, "value", a.ID, "recent", "")
		}
		return "", errc("unknown_crash", []string{param("id", ids[0]), "logcat_crashes"}, "value", a.ID, "recent", strings.Join(ids, ","))
	}
	pk := d.Packages()
	own := procs.NameOwner(pk)
	var su *sharedUID
	if a.Package != "" {
		su = newSharedUID(a.Package, pk, d.Procs)
	}
	var sel []*analysis.Crash
	for _, c := range all {
		if !crashPkgMatch(c, a.Package, own, su) || (len(kinds) > 0 && !kinds[c.Kind]) {
			continue
		}
		if (fromNs != 0 && c.TimeNs < fromNs) || (toNs != 0 && c.TimeNs >= toNs) {
			continue
		}
		sel = append(sel, c)
	}
	var b strings.Builder
	if len(sel) == 1 { // one match: its detail, with the matching note
		c := sel[0]
		if full := d.CrashFull(c.ID)[0]; full != nil {
			c = full
		}
		det := t.crashDetail(d, ci, c, all, a)
		if su != nil {
			head, rest, _ := strings.Cut(det, "\n")
			det = head + "\n" + su.noteLine(a.Verbose) + "\n" + rest
		}
		return det, nil
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
		key := c.Kind + "|" + own(c.Process) + "|" + sig
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
	shown := min(len(groups), a.Limit)
	word := "ok"
	if len(sel) == 0 {
		word = "empty"
	}
	kv := []any{"groups", len(groups), "events", len(sel), "shown", shown}
	if a.Package != "" {
		kv = append(kv, "package", a.Package)
	}
	kv = append(kv, "order", "newest_first")
	if st := d.Store.Stats(); st.OldestNs != 0 {
		kv = append(kv, "history_from", isoNs(st.OldestNs, loc))
	}
	// DropBox sources: only the ones that are not ok matter to the next call
	if bad := dropboxIssues(d); bad != "" {
		kv = append(kv, "dropbox", bad)
	}
	b.WriteString(crashHead(d, ci, a.Verbose, word, "list", kv...))
	if su != nil {
		b.WriteString(su.noteLine(a.Verbose) + "\n")
	}
	if len(sel) == 0 {
		alt := []string{`kinds=["java","native","anr"]`, `from="24h"`}
		code := "no_match"
		if a.Package != "" {
			code = "no_crash_for_package"
			alt = []string{`logcat_search filter="is:crash"`, `package=""`}
		}
		b.WriteString(hdr("hint", "code", code) + nextClause(alt...) + "\n")
		return b.String(), nil
	}
	// the list shows stacks / ANR traces of the shown groups only: decode
	// just those in full (issue #22)
	var heads []string
	for i, g := range groups {
		if i >= a.Limit {
			break
		}
		heads = append(heads, g.items[0].ID)
	}
	fulls := d.CrashFull(heads...)
	n := 0
	for _, g := range groups {
		if n >= a.Limit {
			break
		}
		c := g.items[0]
		if f := fulls[n]; f != nil {
			c = f
		}
		n++
		b.WriteString(crashListEntry(c, loc))
		if len(g.items) > 1 {
			rest := g.items[1:]
			var ids []string
			for _, o := range rest[:min(len(rest), 5)] {
				ids = append(ids, o.ID)
			}
			kv := []any{"same_sig", len(rest), "ids", strings.Join(ids, ",")}
			if len(rest) > 5 {
				kv = append(kv, "ids_more", len(rest)-5)
			}
			kv = append(kv, "first", isoNs(rest[len(rest)-1].TimeNs, loc))
			b.WriteString("  " + hdr("same_signature", kv...) + nextClause(param("id", rest[0].ID)) + "\n")
		}
	}
	if len(groups) > shown {
		older := len(groups) - shown
		next := []string{param("to", isoNs(groups[shown-1].items[0].TimeNs, loc))}
		if a.Limit < 50 {
			next = []string{param("limit", min(50, a.Limit+older)), next[0]}
		}
		b.WriteString(truncLine("older_groups", older, true, nil, next...))
	}
	b.WriteString("next: " + param("id", groups[0].items[0].ID) + "\n")
	return b.String(), nil
}

// dropboxIssues lists the DropBox sources whose last fetch was not ok:
// "tag:status,..." ("" when all are ok).
func dropboxIssues(d *device.Device) string {
	var parts []string
	for _, tag := range []string{"data_app_crash", "data_app_native_crash", "data_app_anr"} {
		s := d.DropboxStatus(tag)
		if i := strings.IndexByte(s, ' '); i > 0 && strings.HasPrefix(s, "20") {
			s = s[i+1:]
		}
		if s = strings.TrimSpace(s); s != "ok" {
			parts = append(parts, tag+":"+noSpace(strings.ReplaceAll(s, ",", ";")))
		}
	}
	return strings.Join(parts, ",")
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

// crashListEntry is one group of the list: a key=value first line (id first)
// and the headline, first frames and, for an ANR, the main thread's state.
func crashListEntry(c *analysis.Crash, loc *time.Location) string {
	var b strings.Builder
	kv := []any{"id", c.ID, "time", isoNs(c.TimeNs, loc), "kind", c.Kind, "pid", c.PID, "process", orQ(c.Process)}
	if c.Thread != "" {
		kv = append(kv, "thread", c.Thread)
	}
	b.WriteString(hdr("crash", kv...) + "\n")
	switch c.Kind {
	case "java":
		lines := strings.Split(strings.TrimSpace(firstNonEmpty(c.Body, c.Headline)), "\n")
		fmt.Fprintf(&b, "  %s\n", lines[0])
		frames := 0
		total := 0
		for _, ln := range lines[1:] {
			if strings.HasPrefix(strings.TrimSpace(ln), "at ") {
				total++
				if frames < 3 {
					fmt.Fprintf(&b, "    %s\n", strings.TrimSpace(ln))
					frames++
				}
			}
		}
		if cause := rootCause(lines); cause != "" && cause != lines[0] {
			fmt.Fprintf(&b, "    ^ root_cause: %s\n", cause)
		}
		if total > frames {
			fmt.Fprintf(&b, "    ^ frames_hidden=%d next: %s\n", total-frames, param("id", c.ID))
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
				fmt.Fprintf(&b, "  main_thread: %s | %s | traces=%s\n", state, top, tracesSource(c))
			}
		} else {
			fmt.Fprintf(&b, "  traces=unavailable next: %s\n", param("id", c.ID))
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
// whole stack exceeds 20k chars; the marker names the call that shows the
// whole stack (next).
func trimStack(s, next string) string {
	if len(s) <= 20000 {
		return s
	}
	var out []string
	var frames []string
	flush := func() {
		if len(frames) > 45 {
			out = append(out, frames[:40]...)
			out = append(out, fmt.Sprintf("        ^ frames_omitted=%d next: %s", len(frames)-45, next))
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

func (t *Tools) crashDetail(d *device.Device, ci device.ClockInfo, c *analysis.Crash, all []*analysis.Crash, a crashArgs) string {
	loc := d.Loc()
	var b strings.Builder
	kv := []any{"id", c.ID, "kind", c.Kind, "time", isoNs(c.TimeNs, loc), "pid", c.PID, "process", orQ(c.Process)}
	if c.Thread != "" {
		kv = append(kv, "thread", c.Thread)
	}
	// the package line names an application id: not a process name that
	// owns no installed package (android.process.acore: issues #44, #70)
	if p := crashPackage(c.Package, c.Process); p != "" && (p != c.Process || isInstalled(d.Packages(), p)) {
		kv = append(kv, "package", p)
	}
	kv = append(kv, "sources", strings.Join(c.Sources, ","), "order", "arrival", "before_lines", a.BeforeLines)
	b.WriteString(crashHead(d, ci, a.Verbose, "ok", "detail", kv...))
	// the call that shows every frame of a stack that was cut
	fullNext := "logcat_search " + param("filter", fmt.Sprintf("pid:%d", c.PID)) + ` stack="full"`
	switch c.Kind {
	case "java":
		b.WriteString("stack:\n")
		b.WriteString(indent(trimStack(firstNonEmpty(c.Body, c.Dropbox, c.Headline), fullNext), "  "))
		if c.Dropbox != "" && c.Body != "" && strings.Contains(c.Body, "stack unavailable") {
			b.WriteString("dropbox entry:\n" + indent(trimStack(c.Dropbox, fullNext), "  "))
		}
	case "native":
		fmt.Fprintf(&b, "signal: %s\n", c.Headline)
		if c.Location != "" {
			fmt.Fprintf(&b, "crash_location: %s\n", c.Location)
		}
		body := c.Body
		src := "logcat"
		dbx := ""
		if !strings.Contains(body, "backtrace:") {
			switch {
			case c.Tombstone != "":
				body, src, dbx = c.Tombstone, "dropbox_tombstone", "SYSTEM_TOMBSTONE"
			case c.Dropbox != "":
				body, src, dbx = c.Dropbox, "dropbox_native_crash", "data_app_native_crash"
			}
		}
		fmt.Fprintf(&b, "tombstone source=%s:\n", src)
		b.WriteString(indent(trimStack(capStr(body, 12000, dropboxCmd(dbx)), fullNext), "  "))
		if c.Tombstone != "" {
			threads := strings.Count(c.Tombstone, "\n--- --- ---") + 1
			fmt.Fprintf(&b, "tombstone_full threads=%d next: %s\n", threads, dropboxCmd("SYSTEM_TOMBSTONE"))
		} else if !strings.Contains(body, "backtrace") {
			b.WriteString(hdr("hint", "code", "no_tombstone_backtrace") + nextClause("adb bugreport") + "\n")
		}
	case "anr":
		fmt.Fprintf(&b, "reason: %s\n", orStr(c.Headline, "?"))
		if c.CPU != "" && a.Verbose {
			b.WriteString("cpu:\n" + indent(c.CPU, "  "))
		}
		if c.Traces != "" {
			main, holder := analysis.MainThreadTrace(c.Traces)
			if main != "" {
				fmt.Fprintf(&b, "main_thread traces=%s:\n", tracesSource(c))
				b.WriteString(indent(capStr(main, 8000, dropboxCmd("data_app_anr")), "  "))
				if holder != "" {
					b.WriteString("lock_holder_thread:\n" + indent(capStr(holder, 6000, dropboxCmd("data_app_anr")), "  "))
				}
			} else {
				b.WriteString("traces_without_main_thread first_part:\n" + indent(capStr(c.Traces, 4000, dropboxCmd("data_app_anr")), "  "))
			}
		} else {
			fmt.Fprintf(&b, "%s\n", hdr("traces", "state", "unavailable", "reason", "no_dropbox_entry", "dropbox", strings.TrimSpace(d.DropboxStatus("data_app_anr")), "retry_after_s", 5)+nextClause(param("id", c.ID), dropboxCmd("data_app_anr")))
		}
		if c.Body != "" && !strings.Contains(c.Body, "CPU usage") {
			b.WriteString("activity_manager_block:\n" + indent(capStr(c.Body, 3000, dropboxCmd("data_app_anr")), "  "))
		}
	}
	// same-signature history
	var same []string
	for _, o := range all {
		if o.ID != c.ID && o.Kind == c.Kind && o.Signature != "" && o.Signature == c.Signature {
			same = append(same, o.ID)
		}
	}
	if len(same) > 0 {
		kv := []any{"same_sig", len(same), "ids", strings.Join(same[:min(len(same), 8)], ",")}
		if len(same) > 8 {
			kv = append(kv, "ids_more", len(same)-8)
		}
		b.WriteString(hdr("same_signature", kv...) + nextClause(param("id", same[0])) + "\n")
	}
	// the last before_lines lines of the crashed process before the crash
	if c.Seq == 0 || c.PID == 0 {
		b.WriteString(hdr("before_crash", "lines", 0, "reason", "not_in_recorded_logs") + "\n")
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
		return len(before) < a.BeforeLines && scanned < 300000
	})
	b.WriteString(hdr("before_crash", "pid", c.PID, "lines", len(before), "columns", "time level pid/tid tag: message") + "\n")
	w := &lineWriter{loc: loc, noProc: true}
	for i := len(before) - 1; i >= 0; i-- {
		w.add(before[i], false)
	}
	w.flushRepeat()
	for _, l := range w.lines {
		b.WriteString("  " + l + "\n")
	}
	if len(before) == a.BeforeLines && a.BeforeLines < beforeLinesMax {
		b.WriteString(truncLine("before_lines", len(before), false, nil, param("before_lines", min(beforeLinesMax, a.BeforeLines*2))))
	}
	out := b.String()
	if len(out) > outputBudget {
		cut := cutAt(out, outputBudget)
		out = out[:cut] + "\n" + truncLine("detail_chars", len(out)-cut, true, nil, param("before_lines", max(1, a.BeforeLines/2)), fullNext)
	}
	return out
}

// dropboxCmd is the adb command that prints a DropBox entry in full.
func dropboxCmd(tag string) string {
	return "adb shell dumpsys dropbox --print " + tag
}

func indent(s, p string) string {
	s = strings.TrimRight(s, "\n")
	return p + strings.ReplaceAll(s, "\n", "\n"+p) + "\n"
}

// capStr cuts s to n chars with a marker that names the command showing it whole.
func capStr(s string, n int, next string) string {
	if len(s) <= n {
		return s
	}
	cut := cutAt(s, n)
	return s[:cut] + "\n" + lineCut(n, len(s)-cut, next)
}

var _ = sort.Strings

func isInstalled(uidPkgs map[int][]string, pkg string) bool {
	for _, l := range uidPkgs {
		for _, p := range l {
			if p == pkg {
				return true
			}
		}
	}
	return false
}
