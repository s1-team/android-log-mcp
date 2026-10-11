package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func launchRelevant(r *model.Record) bool { return analysis.LaunchTag(r) }

func (t *Tools) launches(ctx context.Context, a launchArgs) (out string, err error) {
	if a.Limit == 0 {
		a.Limit = 5
	}
	if a.Limit < 1 || a.Limit > 20 {
		return "", errf("limit must be 1..20 (got %d)", a.Limit)
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
	loc := d.Loc()
	ci, fromNs, toNs, err := window(d, a.From, a.To, 30*time.Minute)
	if err != nil {
		return "", err
	}
	now := ci.Now
	var recs []*model.Record
	start := time.Now()
	stopped := false
	scanTo := int64(0)
	if toNs != 0 {
		scanTo = toNs + analysis.DisplayedSlackNs
	}
	// every record in the window, however it is stored (device clock
	// changes): no early stop on record order (issue #48)
	d.Store.ScanTime(0, 0, true, fromNs, scanTo, func(r *model.Record) bool {
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
	jumps := d.Store.ClockJumps()
	steps := make([]analysis.ClockStep, len(jumps))
	for i, j := range jumps {
		steps[i] = analysis.ClockStep{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}
	if len(jumps) > 0 && !stopped {
		recs, stopped = afterJumps(d.Store, recs, jumps, start)
	}
	ls := analysis.LaunchesAcross(recs, a.Package, steps)
	// requests in the window only (the lines after a clock jump can hold
	// requests outside it)
	keep := ls[:0]
	for _, l := range ls {
		if l.RequestNs >= fromNs && (toNs == 0 || l.RequestNs < toNs) {
			keep = append(keep, l)
		}
	}
	ls = keep
	// newest first
	for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
		ls[i], ls[j] = ls[j], ls[i]
	}
	var b strings.Builder
	scope := "all apps"
	if a.Package != "" {
		scope = "package " + a.Package
	}
	fmt.Fprintf(&b, "device %s · device time %s · %s · %s · %d launches since %s\n", d.Serial, tzLabel(loc, d.TZName(), now), clockLine(ci), scope, len(ls), dateTime(fromNs, loc))
	if len(ls) == 0 {
		b.WriteString("no launch requests (ActivityTaskManager \"START u0 … cmp=<pkg>/…\") found in range.\n")
		if st := d.Store.Stats(); st.OldestNs > fromNs {
			fmt.Fprintf(&b, "note: history starts %s.\n", dateTime(st.OldestNs, loc))
		}
		b.WriteString("Launch the app first (adb shell am force-stop <pkg>; adb shell am start -W -n <pkg>/<activity>, or tap the icon), or widen from.\n")
		return b.String(), nil
	}
	b.WriteString(launchJumps(recs, jumps, loc))
	for i, l := range ls {
		if i >= a.Limit {
			fmt.Fprintf(&b, "[%d older launches not shown; raise limit]\n", len(ls)-a.Limit)
			break
		}
		b.WriteString(renderLaunch(i+1, l, loc))
	}
	b.WriteString("how to read: \"+N ms\" = when the line was logged, relative to the launch request. onX lines are logged when the callback returned. TTID/TTFD are the system's own measurement (request → first frame / reportFullyDrawn) and can be logged much later than they happened.\n")
	for i, l := range ls {
		if i < a.Limit && l.ClockJumped {
			b.WriteString("note: steps marked \"?\" (and a TTID/TTFD marked unreliable) were logged after the device clock was set (clock jump), so their device times cannot be subtracted from the request's: that duration is not shown. The TTID/TTFD and \"callback took\" values themselves are measured by the system on a monotonic clock and stay valid.\n")
			break
		}
	}
	if stopped {
		b.WriteString("note: scan stopped early to stay responsive; narrow from/to.\n")
	}
	return b.String(), nil
}

func renderLaunch(n int, l *analysis.Launch, loc *time.Location) string {
	var b strings.Builder
	pid := ""
	if l.PID != 0 {
		pid = fmt.Sprintf("  (pid %d)", l.PID)
	}
	why := ""
	if l.KindWhy != "" {
		why = " — " + l.KindWhy
	}
	fmt.Fprintf(&b, "#%d %s  %s  %s%s%s\n", n, time.Unix(0, l.RequestNs).In(loc).Format("2006-01-02 15:04:05.000"), l.Kind, l.Component, pid, why)
	rel := func(ns int64) string { return fmt.Sprintf("+%d ms", (ns-l.RequestNs)/1e6) }
	// a step logged after a clock jump that followed the request: no
	// duration, only its device time (issue #54)
	when := func(s analysis.Step) string {
		if s.AfterJump {
			return "?"
		}
		return rel(s.Ns)
	}
	jumped := func(s analysis.Step) string {
		return unreliable + "; logged at device time " + time.Unix(0, s.Ns).In(loc).Format("2006-01-02 15:04:05.000")
	}
	row := func(when, what, extra string) {
		fmt.Fprintf(&b, "   %-10s %-22s %s\n", when, what, extra)
	}
	req := l.RequestMsg
	if len(req) > 160 {
		req = req[:160] + "…"
	}
	row("+0 ms", "launch request", req)
	took := func(s analysis.Step) string {
		if s.TookMs >= 0 {
			return fmt.Sprintf("(callback took %d ms)", s.TookMs)
		}
		return ""
	}
	if l.ProcStart.Ns != 0 {
		note := l.ProcStart.Note
		if l.ProcStart.AfterJump {
			note += "   " + jumped(l.ProcStart)
		}
		row(when(l.ProcStart), "process started", note)
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
			row("—", st.name, "not logged")
			continue
		}
		extra := "returned " + took(st.s)
		switch {
		case st.s.AfterJump:
			extra += "   " + jumped(st.s)
		case st.name == "onResume":
			extra += "   ← request→onResume " + strings.TrimPrefix(rel(st.s.Ns), "+")
		}
		row(when(st.s), st.name, strings.TrimSpace(extra))
	}
	if l.Displayed.Ns != 0 {
		at := "logged at " + rel(l.Displayed.Ns)
		if l.Displayed.AfterJump {
			at = jumped(l.Displayed)
		}
		fmt.Fprintf(&b, "   TTID (time to initial display, system-measured): %d ms   [source: %s, %s]\n", l.Displayed.TookMs, l.Displayed.Note, at)
	} else {
		fmt.Fprintf(&b, "   TTID: not logged\n")
	}
	if l.FullyDrawn.Ns != 0 {
		at := "logged at " + rel(l.FullyDrawn.Ns)
		if l.FullyDrawn.AfterJump {
			at = jumped(l.FullyDrawn)
		}
		fmt.Fprintf(&b, "   TTFD (time to full display, reportFullyDrawn): %d ms   [%s]\n", l.FullyDrawn.TookMs, at)
	} else {
		fmt.Fprintf(&b, "   TTFD: not reported (app does not call reportFullyDrawn(), or not yet)\n")
	}
	for _, n := range l.Notes {
		fmt.Fprintf(&b, "   note: %s\n", n)
	}
	return b.String()
}

// unreliable replaces a duration across a device clock jump (issue #54).
const unreliable = "duration: unreliable (device clock jumped)"

// afterJumps adds the launch lines logged just after a device clock jump
// that follows a launch request in recs (arrival order): their device
// times can lie outside the queried window (a back jump to 1970, a
// forward jump of a year), but they belong to that request. For each such
// jump it reads, by seq, the lines up to analysis.DisplayedSlackNs of
// device time after it (device time steps larger than a clock jump do not
// count). Returns recs in arrival order and whether the scan budget ran
// out.
func afterJumps(st *store.Store, recs []*model.Record, jumps []store.ClockJump, start time.Time) ([]*model.Record, bool) {
	// continuous time: device time minus the jumps before it
	cum := make([]int64, len(jumps)+1)
	for i, j := range jumps {
		cum[i+1] = cum[i] + j.DeltaNs
	}
	seg := func(seq uint64) int { return sort.Search(len(jumps), func(i int) bool { return jumps[i].Seq > seq }) }
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
	for ji, j := range jumps {
		near := false
		for _, r := range reqs {
			if r.Seq >= j.Seq {
				continue
			}
			c := r.TimeNs - cum[seg(r.Seq)]
			at := j.AfterNs - cum[ji+1] // continuous time of the jump
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

// launchJumps: the clock: / clock_jump: lines of the device clock jumps
// among the scanned launch lines (the newest 3).
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
	if len(in) > 3 {
		fmt.Fprintf(&b, "clock: %d device clock jumps among the scanned launch lines; the newest 3:\n", len(in))
		in = in[len(in)-3:]
	}
	for i := len(in) - 1; i >= 0; i-- {
		b.WriteString(jumpLines(in[i], loc, -1, -1, ""))
	}
	return b.String()
}
