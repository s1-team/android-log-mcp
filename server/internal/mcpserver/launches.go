package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	eventclock "github.com/s1crab/android-log-mcp/server/internal/clock"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func launchRelevant(r *model.Record) bool { return analysis.LaunchTag(r) }

func (t *Tools) launches(ctx context.Context, a launchArgs) (out string, err error) {
	if a.Limit == 0 {
		a.Limit = 5
	}
	if a.Limit < 1 || a.Limit > 20 {
		return "", errc("bad_param", []string{"limit=20"}, "param", "limit", "value", a.Limit, "valid", "1..20")
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
	ci, fromNs, toNs, err := window(d, a.From, a.To, 30*time.Minute)
	if err != nil {
		return "", err
	}
	if err := checkPackages(a.Package); err != nil {
		return "", err
	}
	jumps := d.Store.ClockJumps()
	steps := make([]analysis.ClockStep, len(jumps))
	for i, j := range jumps {
		steps[i] = analysis.ClockStep{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}

	// #270: prefer launches.jsonl index; lazy backfill; ScanTime fallback.
	var ls []*analysis.Launch
	var stopped bool
	var backfillNote string
	usedIndex := false
	incomplete, bnote := device.EnsureLaunches(d.Store, scanBudget)
	if incomplete && bnote != "" {
		backfillNote = bnote
	}
	if !incomplete && device.LaunchIndexReady(d.Store) {
		sums := device.LaunchSummaries(d.Store)
		var ids []string
		pkgSet := packageSet(a.Package)
		for _, s := range sums {
			if pkgSet != nil && !pkgSet[s.Package] {
				continue
			}
			if s.RequestNs < fromNs || (toNs != 0 && s.RequestNs >= toNs) {
				continue
			}
			ids = append(ids, s.ID)
		}
		full := device.LaunchFull(d.Store, ids...)
		for _, l := range full {
			if l != nil {
				ls = append(ls, l)
			}
		}
		usedIndex = true
	}
	if !usedIndex {
		ls, stopped = launchesScanTime(d.Store, a.Package, fromNs, toNs, steps, jumps)
	}
	// recompute AfterJump from current jumps (strategy B), index or scan
	for _, l := range ls {
		analysis.ApplyLaunchJumps(l, steps)
	}

	var b strings.Builder
	word := "ok"
	if len(ls) == 0 {
		word = "empty"
	}
	shown := min(len(ls), a.Limit)
	kv := []any{"view", "launches", "device", d.Serial, "launches", len(ls), "shown", shown, "order", "newest_first", "since", isoNs(fromNs, loc)}
	var crashNote string
	if len(a.Package) > 0 { // #269/#276: same effective window as launches (incl. default from=30m)
		var pckv []any
		pckv, crashNote = packageCrashAttach(d, a.Package, true, packageCrashBounds{fromNs: fromNs, toNs: toNs})
		kv = append(kv, pckv...)
	}
	if a.Verbose {
		kv = append(kv, "tz", d.TZName())
	}
	kv = append(kv, clockKV(ci, a.Verbose)...)
	if shown > 0 && !stopped { // callback durations are missing on this Android version: said once
		for _, l := range ls[:shown] {
			if l.Events == "am_on_*" {
				kv = append(kv, "note", "no_callback_durations")
				break
			}
		}
	}
	kv = append(kv, "fmt", fmtVersion)
	b.WriteString(hdr(word, kv...) + "\n")
	b.WriteString(crashNote)
	if backfillNote != "" {
		b.WriteString(hdr("note", "code", backfillNote) + "\n")
	}
	if len(ls) == 0 {
		if st := d.Store.Stats(); st.OldestNs > fromNs {
			b.WriteString(hdr("note", "code", "history_starts", "oldest", isoNs(st.OldestNs, loc)) + "\n")
		}
		next := []string{`from="2h"`}
		if len(a.Package) > 0 {
			next = []string{"adb shell monkey -p " + a.Package[0] + " -c android.intent.category.LAUNCHER 1", next[0]}
		}
		b.WriteString(hdr("hint", "code", "no_launch_requests") + nextClause(next...) + "\n")
		return b.String(), nil
	}
	// jump lines among shown launches from stored seq range + meta jumps (#270)
	shownLs := ls[:shown]
	b.WriteString(launchJumpsFromLaunches(shownLs, jumps, loc))
	for i, l := range ls {
		if i >= a.Limit {
			older := len(ls) - a.Limit
			next := param("limit", min(20, len(ls)))
			if len(ls) > 20 {
				next = param("to", isoNs(ls[a.Limit-1].RequestNs, loc))
			}
			b.WriteString(truncLine("older", older, true, nil, next))
			break
		}
		b.WriteString(renderLaunch(i+1, l, loc))
	}
	if stopped {
		b.WriteString(truncLine("scan_stopped_ms", int(scanBudget.Milliseconds()), false, nil, `from="10m"`))
	}
	return b.String(), nil
}

// launchesScanTime is the pre-index path: ScanTime + LaunchesAcross (#270 fallback).
func launchesScanTime(st *store.Store, pkgs []string, fromNs, toNs int64, steps []analysis.ClockStep, jumps []store.ClockJump) ([]*analysis.Launch, bool) {
	var recs []*model.Record
	start := time.Now()
	stopped := false
	scanTo := int64(0)
	if toNs != 0 {
		scanTo = toNs + analysis.DisplayedSlackNs
	}
	st.ScanTime(0, 0, true, fromNs, scanTo, func(r *model.Record) bool {
		if time.Since(start) > scanBudget {
			stopped = true
			return false
		}
		if r.TimeNs >= fromNs && (toNs == 0 || r.TimeNs < toNs+analysis.DisplayedSlackNs) && launchRelevant(r) {
			recs = append(recs, r)
		}
		return true
	})
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	if len(jumps) > 0 && !stopped {
		recs, stopped = afterJumps(st, recs, jumps, start)
	}
	filterPkg := ""
	if len(pkgs) == 1 {
		filterPkg = pkgs[0]
	}
	ls := analysis.LaunchesAcross(recs, filterPkg, steps)
	set := packageSet(pkgs)
	keep := ls[:0]
	for _, l := range ls {
		if set != nil && !set[l.Package] {
			continue
		}
		if l.RequestNs >= fromNs && (toNs == 0 || l.RequestNs < toNs) {
			keep = append(keep, l)
		}
	}
	ls = keep
	for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
		ls[i], ls[j] = ls[j], ls[i]
	}
	return ls, stopped
}

// launchJumpsFromLaunches: clock jumps whose Seq falls in the shown launches'
// request/step seq range (meta jumps; #270 decision 6).
func launchJumpsFromLaunches(ls []*analysis.Launch, jumps []store.ClockJump, loc *time.Location) string {
	if len(ls) == 0 {
		return ""
	}
	lo, hi := uint64(0), uint64(0)
	for _, l := range ls {
		if l == nil || l.Seq == 0 {
			continue
		}
		if lo == 0 || l.Seq < lo {
			lo = l.Seq
		}
		if l.Seq > hi {
			hi = l.Seq
		}
		for _, s := range []analysis.Step{l.ProcStart, l.OnCreate, l.OnRestart, l.OnStart, l.OnResume, l.Displayed, l.FullyDrawn} {
			if s.Seq != 0 {
				if s.Seq < lo {
					lo = s.Seq
				}
				if s.Seq > hi {
					hi = s.Seq
				}
			}
		}
	}
	if lo == 0 {
		return ""
	}
	var in []store.ClockJump
	for _, j := range jumps {
		if j.Seq > lo && j.Seq <= hi {
			in = append(in, j)
		}
	}
	if len(in) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "clock_jumps=%d shown=%d\n", len(in), min(len(in), 3))
	if len(in) > 3 {
		in = in[len(in)-3:]
	}
	for i := len(in) - 1; i >= 0; i-- {
		b.WriteString(jumpLines(in[i], loc, -1, -1, ""))
	}
	return b.String()
}

// launchNoteCodes maps the analysis' explanations to codes (the sentences
// live in docs/reference.md, not in every response).
var launchNoteCodes = []struct{ prefix, code string }{
	{"no lifecycle or Displayed evidence", "no_lifecycle_evidence"},
	{"no Displayed line: nothing new was drawn", "no_new_draw"},
	{"no Displayed line: expected for HOT", "no_displayed_hot"},
	{"Displayed not logged (yet)", "displayed_pending"},
}

func renderLaunch(n int, l *analysis.Launch, loc *time.Location) string {
	var b strings.Builder
	kv := []any{"time", isoNs(l.RequestNs, loc), "kind", l.Kind, "component", l.Component}
	if l.PID != 0 {
		kv = append(kv, "pid", l.PID)
	}
	if l.KindWhy != "" {
		kv = append(kv, "why", strings.ReplaceAll(l.KindWhy, "→", "->"))
	}
	if l.Displayed.Ns != 0 {
		kv = append(kv, "ttid_ms", l.Displayed.TookMs)
	} else {
		kv = append(kv, "ttid", "none")
	}
	if l.FullyDrawn.Ns != 0 {
		kv = append(kv, "ttfd_ms", l.FullyDrawn.TookMs)
	} else {
		kv = append(kv, "ttfd", "none")
	}
	fmt.Fprintf(&b, "#%d %s\n", n, hdr("launch", kv...))
	rel := func(ns int64) int64 { return (ns - l.RequestNs) / 1e6 }
	// a step logged after a clock jump that followed the request: no
	// duration, only its device time (issue #54)
	at := func(s analysis.Step) []any {
		if s.AfterJump {
			return []any{"at_ms", "?", "unreliable", "clock_jump", "device_time", isoNs(s.Ns, loc)}
		}
		return []any{"at_ms", rel(s.Ns)}
	}
	step := func(name string, kv ...any) {
		b.WriteString("  " + hdr("step", append([]any{"name", name}, kv...)...) + "\n")
	}
	req := l.RequestMsg
	if len(req) > 160 {
		req = req[:cutAt(req, 160)] + "..."
	}
	step("launch_request", "at_ms", 0, "msg", req)
	took := func(s analysis.Step) []any {
		if s.TookMs >= 0 {
			return []any{"took_ms", s.TookMs}
		}
		return nil
	}
	if l.ProcStart.Ns != 0 {
		kv := at(l.ProcStart)
		if l.ProcStart.Note != "" {
			kv = append(kv, "note", l.ProcStart.Note)
		}
		step("process_started", kv...)
	}
	short := l.Class
	if i := strings.LastIndexByte(short, '.'); i >= 0 {
		short = short[i+1:]
	}
	steps := []struct {
		name string
		s    analysis.Step
	}{{short + ".onCreate", l.OnCreate}, {"onRestart", l.OnRestart}, {"onStart", l.OnStart}, {"onResume", l.OnResume}}
	for _, st := range steps {
		if st.s.Ns == 0 {
			if (st.name == "onRestart" && l.Kind != "HOT") || (strings.HasSuffix(st.name, ".onCreate") && l.Kind == "HOT") {
				continue // not expected for this kind
			}
			step(st.name, "logged", 0)
			continue
		}
		step(st.name, append(at(st.s), took(st.s)...)...)
	}
	if l.Displayed.Ns != 0 {
		kv := []any{"ms", l.Displayed.TookMs, "source", l.Displayed.Note}
		if l.Displayed.AfterJump {
			kv = append(kv, "unreliable", "clock_jump", "device_time", isoNs(l.Displayed.Ns, loc))
		} else {
			kv = append(kv, "logged_at_ms", rel(l.Displayed.Ns))
		}
		b.WriteString("  " + hdr("ttid", kv...) + "\n")
	}
	if l.FullyDrawn.Ns != 0 {
		kv := []any{"ms", l.FullyDrawn.TookMs}
		if l.FullyDrawn.AfterJump {
			kv = append(kv, "unreliable", "clock_jump", "device_time", isoNs(l.FullyDrawn.Ns, loc))
		} else {
			kv = append(kv, "logged_at_ms", rel(l.FullyDrawn.Ns))
		}
		b.WriteString("  " + hdr("ttfd", kv...) + "\n")
	}
	for _, n := range l.Notes {
		if n == "this Android version logs am_on_*_called without callback durations" {
			continue // note=no_callback_durations in the header
		}
		code := ""
		for _, c := range launchNoteCodes {
			if strings.HasPrefix(n, c.prefix) {
				code = c.code
			}
		}
		if code == "" {
			b.WriteString("  " + hdr("note", "code", "other", "msg", n) + "\n")
		} else {
			b.WriteString("  " + hdr("note", "code", code) + "\n")
		}
	}
	return b.String()
}

// afterJumps adds the launch lines logged just after a device clock jump
// that follows a launch request in recs (arrival order): their device
// times can lie outside the queried window (a back jump to 1970, a
// forward jump of a year), but they belong to that request. For each such
// jump it reads, by seq, the lines up to analysis.DisplayedSlackNs of
// device time after it (device time steps larger than a clock jump do not
// count). Returns recs in arrival order and whether the scan budget ran
// out.
func afterJumps(st *store.Store, recs []*model.Record, jumps []store.ClockJump, start time.Time) ([]*model.Record, bool) {
	// continuous time via EventClock (R1 #258); possible jumps excluded
	js := make([]eventclock.Jump, len(jumps))
	for i, j := range jumps {
		js[i] = eventclock.Jump{Seq: j.Seq, DeltaNs: j.DeltaNs, Source: j.Source}
	}
	ec := eventclock.NewEventClock(js)
	cont := func(seq uint64, tNs int64) int64 {
		return ec.Continuous(ec.Stamp(eventclock.ArrivalSeq(seq), eventclock.DeviceInstant(tNs)))
	}
	var reqs []*model.Record
	for _, r := range recs {
		if analysis.LaunchRequest(r) {
			reqs = append(reqs, r)
		}
	}
	have := make(map[uint64]bool, len(recs))
	for _, r := range recs {
		have[r.Seq] = true
	}
	stopped := false
	added := false
	for _, j := range jumps {
		if j.Source == "possible" {
			continue
		}
		near := false
		for _, r := range reqs {
			if r.Seq >= j.Seq {
				continue
			}
			c := cont(r.Seq, r.TimeNs)
			at := cont(j.Seq, j.AfterNs) // continuous time of the jump
			if j.AfterNs == 0 || (at >= c-store.ClockJumpThreshold && at-c <= analysis.DisplayedSlackNs) {
				near = true
				break
			}
		}
		if !near {
			continue
		}
		var prev, elapsed int64
		st.Scan(j.Seq-1, 0, false, func(r *model.Record) bool {
			if time.Since(start) > scanBudget {
				stopped = true
				return false
			}
			if prev != 0 {
				if d := r.TimeNs - prev; d > 0 && d <= store.ClockJumpThreshold {
					elapsed += d
				}
			}
			prev = r.TimeNs
			if elapsed > analysis.DisplayedSlackNs {
				return false
			}
			if !have[r.Seq] && analysis.LaunchTag(r) {
				have[r.Seq] = true
				recs = append(recs, r)
				added = true
			}
			return true
		})
		if stopped {
			break
		}
	}
	if added {
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	}
	return recs, stopped
}

// launchJumps: the clock jumps among the scanned launch lines (the newest
// 3, as clock_jump lines; clock_jumps=N first when there are more).
func launchJumps(recs []*model.Record, jumps []store.ClockJump, loc *time.Location) string {
	if len(recs) == 0 {
		return ""
	}
	lo, hi := recs[0].Seq, recs[len(recs)-1].Seq
	var in []store.ClockJump
	for _, j := range jumps {
		if j.Seq > lo && j.Seq <= hi {
			in = append(in, j)
		}
	}
	if len(in) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "clock_jumps=%d shown=%d\n", len(in), min(len(in), 3))
	if len(in) > 3 {
		in = in[len(in)-3:]
	}
	for i := len(in) - 1; i >= 0; i-- {
		b.WriteString(jumpLines(in[i], loc, -1, -1, ""))
	}
	return b.String()
}
