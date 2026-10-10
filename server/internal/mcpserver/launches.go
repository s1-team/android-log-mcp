package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/timeparse"
)

func launchRelevant(r *model.Record) bool { return analysis.LaunchTag(r) }

func (t *Tools) launches(ctx context.Context, a launchArgs) (string, error) {
	if a.Limit == 0 {
		a.Limit = 5
	}
	if a.Limit < 1 || a.Limit > 20 {
		return "", errf("limit must be 1..20 (got %d)", a.Limit)
	}
	d, err := t.resolveDevice(a.Device)
	if err != nil {
		return "", err
	}
	d.Store.Refresh()
	loc := d.Loc()
	now := d.Now()
	fromNs := now.Add(-30 * time.Minute).UnixNano()
	var toNs int64
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
	var recs []*model.Record
	start := time.Now()
	stopped := false
	d.Store.Scan(0, 0, true, func(r *model.Record) bool {
		if r.TimeNs < fromNs-timeSlackNs {
			return false
		}
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
	ls := analysis.Launches(recs, a.Package)
	if toNs != 0 {
		var keep []*analysis.Launch
		for _, l := range ls {
			if l.RequestNs < toNs {
				keep = append(keep, l)
			}
		}
		ls = keep
	}
	// newest first
	for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
		ls[i], ls[j] = ls[j], ls[i]
	}
	var b strings.Builder
	scope := "all apps"
	if a.Package != "" {
		scope = "package " + a.Package
	}
	fmt.Fprintf(&b, "device %s · %s · %s · %d launches since %s\n", d.Serial, tzLabel(loc, d.TZName(), now), scope, len(ls), dateTime(fromNs, loc))
	if len(ls) == 0 {
		b.WriteString("no launch requests (ActivityTaskManager \"START u0 … cmp=<pkg>/…\") found in range.\n")
		if st := d.Store.Stats(); st.OldestNs > fromNs {
			fmt.Fprintf(&b, "note: history starts %s.\n", dateTime(st.OldestNs, loc))
		}
		b.WriteString("Launch the app first (adb shell am force-stop <pkg>; adb shell am start -W -n <pkg>/<activity>, or tap the icon), or widen from.\n")
		return b.String(), nil
	}
	for i, l := range ls {
		if i >= a.Limit {
			fmt.Fprintf(&b, "[%d older launches not shown; raise limit]\n", len(ls)-a.Limit)
			break
		}
		b.WriteString(renderLaunch(i+1, l, loc))
	}
	b.WriteString("how to read: \"+N ms\" = when the line was logged, relative to the launch request. onX lines are logged when the callback returned. TTID/TTFD are the system's own measurement (request → first frame / reportFullyDrawn) and can be logged much later than they happened.\n")
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
		row(rel(l.ProcStart.Ns), "process started", l.ProcStart.Note)
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
		if st.name == "onResume" {
			extra += "   ← request→onResume " + strings.TrimPrefix(rel(st.s.Ns), "+")
		}
		row(rel(st.s.Ns), st.name, strings.TrimSpace(extra))
	}
	if l.Displayed.Ns != 0 {
		fmt.Fprintf(&b, "   TTID (time to initial display, system-measured): %d ms   [source: %s, logged at %s]\n", l.Displayed.TookMs, l.Displayed.Note, rel(l.Displayed.Ns))
	} else {
		fmt.Fprintf(&b, "   TTID: not logged\n")
	}
	if l.FullyDrawn.Ns != 0 {
		fmt.Fprintf(&b, "   TTFD (time to full display, reportFullyDrawn): %d ms   [logged at %s]\n", l.FullyDrawn.TookMs, rel(l.FullyDrawn.Ns))
	} else {
		fmt.Fprintf(&b, "   TTFD: not reported (app does not call reportFullyDrawn(), or not yet)\n")
	}
	for _, n := range l.Notes {
		fmt.Fprintf(&b, "   note: %s\n", n)
	}
	return b.String()
}
