package device

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
)

// Backfill speed (issue #11 follow-up): no side work while backfilling,
// exec-out streaming, the merged identity + recorder probe, and the
// estimated total in the progress note.

func crashLine(t time.Time, pid int) string {
	return fmt.Sprintf("%10d.%06d %5d %5d %5d E AndroidRuntime: FATAL EXCEPTION: main", t.Unix(), t.Nanosecond()/1000, 10146, pid, pid)
}

// slowBackfill creates FAKEA with n history lines (two app crashes among
// them, unknown pids) streaming at delay per line.
func slowBackfill(t *testing.T, e fakeadb.Env, n int, delay time.Duration) {
	t.Helper()
	addDevices(t, e, 0, "FAKEA")
	now := time.Now()
	var lines []string
	for i := 0; i < n; i++ {
		at := now.Add(time.Duration(i-n) * 10 * time.Millisecond)
		if i == 3 || i == n/2 {
			lines = append(lines, crashLine(at, 4000+i))
			continue
		}
		lines = append(lines, fakeadb.Line(at, 200+i, "PRE", fmt.Sprintf("line %d", i)))
	}
	if err := e.Log("FAKEA", lines...); err != nil {
		t.Fatal(err)
	}
	e.SetDelay("FAKEA", delay)
}

// shortDropbox makes DropBox fetches of live crashes start at once.
func shortDropbox(t *testing.T) {
	testTiming(t).DropboxDelay.Set(50 * time.Millisecond)
}

func callsSince(e fakeadb.Env, since time.Time, sub string) (before, after []string) {
	for _, c := range e.CallLog() {
		if !strings.Contains(c.Args, sub) {
			continue
		}
		if c.At.Before(since) {
			before = append(before, c.Args)
		} else {
			after = append(after, c.Args)
		}
	}
	return
}

func dropboxCounts(e fakeadb.Env) map[string]int {
	n := map[string]int{}
	for _, c := range e.CallsMatching("dumpsys dropbox --print ") {
		n[c[strings.LastIndex(c, " ")+1:]]++
	}
	return n
}

// While the buffer streams no dumpsys / ps / pm runs; once caught up the
// process table is seeded and each DropBox tag is fetched exactly once
// (the crash tags found in the backfill merged with the new-boot history).
func TestNoSideWorkDuringBackfill(t *testing.T) {
	t.Parallel() // the DropBox delay is per test (testTiming), nothing package-wide is touched
	shortDropbox(t)
	e := fakeEnv(t)
	slowBackfill(t, e, 40, 40*time.Millisecond) // ~1.6 s
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	caught := d.caughtUp()
	var at time.Time
	select {
	case <-caught:
		at = time.Now()
	case <-time.After(20 * time.Second):
		t.Fatal("not caught up")
	}
	at = at.Add(-20 * time.Millisecond) // the fake stamps calls after it started
	for _, sub := range []string{"dumpsys", " ps", "pm list", "head -n"} {
		if before, _ := callsSince(e, at, sub); len(before) > 0 {
			t.Errorf("%q ran during the backfill: %q", sub, before)
		}
	}
	if len(messages(d.Store)) != 40 {
		t.Fatalf("stored %d lines", len(messages(d.Store)))
	}
	waitFor(t, "the DropBox history fetch", 10*time.Second, func() bool { return len(dropboxCounts(e)) == len(dropboxHistory) })
	waitFor(t, "ps after catch-up", 10*time.Second, func() bool { _, a := callsSince(e, at, " ps"); return len(a) > 0 })
	time.Sleep(500 * time.Millisecond)
	for tag, n := range dropboxCounts(e) {
		if n != 1 {
			t.Errorf("%s fetched %d times", tag, n)
		}
	}
	if _, a := callsSince(e, at, " ps"); len(a) != 1 {
		t.Errorf("history pids seeded ps: %q", a) // only psLoop's own seed
	}

	// a live crash after catch-up is fetched (again) right away; a pid
	// logged now (after the probe) seeds ps
	e.Log("FAKEA", crashLine(time.Now(), 9000))
	waitFor(t, "the live crash's DropBox fetch", 10*time.Second, func() bool { return dropboxCounts(e)["data_app_crash"] == 2 })
	waitFor(t, "ps for a live unknown pid", 10*time.Second, func() bool { _, a := callsSince(e, at, " ps"); return len(a) == 2 })
}

// A crash found by a resumed (same boot) backfill asks for its tag only
// after catch-up, and not at all when the tag was fetched recently.
func TestDropboxAfterResume(t *testing.T) {
	t.Parallel() // the DropBox delay is per test (testTiming), nothing package-wide is touched
	shortDropbox(t)
	e := fakeEnv(t)
	slowBackfill(t, e, 10, 0)
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "the DropBox history fetch", 10*time.Second, func() bool { return len(dropboxCounts(e)) == len(dropboxHistory) })
	m.Unwant("FAKEA", "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })

	e.Log("FAKEA", crashLine(time.Now(), 9100))
	d = startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	time.Sleep(700 * time.Millisecond)
	if n := dropboxCounts(e)["data_app_crash"]; n != 1 {
		t.Fatalf("data_app_crash fetched %d times; fetched a minute ago, so the resumed backfill's crash needs none", n)
	}

	testTiming(t).DropboxRecent.Set(0)
	m.Unwant("FAKEA", "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
	e.Log("FAKEA", crashLine(time.Now(), 9200))
	d = startDevice(t, m, "FAKEA")
	d.WaitCaughtUp(context.Background(), 10*time.Second)
	waitFor(t, "the deferred fetch", 10*time.Second, func() bool { return dropboxCounts(e)["data_app_crash"] == 2 })
	time.Sleep(500 * time.Millisecond)
	if n := dropboxCounts(e); n["data_app_crash"] != 2 || n["data_app_anr"] != 1 {
		t.Fatalf("fetches %v (same boot: only the crash's tag, once)", n)
	}
}

// Side work does not wait forever for a backfill that never catches up.
func TestSideWorkWaitCap(t *testing.T) {
	t.Parallel()
	testTiming(t).SideWorkWait.Set(300 * time.Millisecond)
	e := fakeEnv(t)
	slowBackfill(t, e, 200, 50*time.Millisecond) // ~10 s
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "ps before catch-up", 5*time.Second, func() bool { return len(e.CallsMatching(" ps")) > 0 })
	if !d.CatchUp().Pending {
		t.Fatal("caught up already; the cap was not exercised")
	}
}

// The stream is `adb exec-out logcat`.
func TestStreamExecOut(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	if c := e.CallsMatching("-s FAKEA exec-out logcat -b main,system,crash,events"); len(c) != 1 {
		t.Fatalf("exec-out streams %q", c)
	}
	if c := e.CallsMatching("-s FAKEA logcat -b"); len(c) != 0 {
		t.Fatalf("adb logcat used: %q", c)
	}
	m.Unwant("FAKEA", "test")
	waitFor(t, "the exec-out stream to be killed", 5*time.Second, func() bool { return !e.LogcatRunning("FAKEA") })
}

// adbd without exec (API < 21): exec-out fails before the first line and
// the recorder falls back to `adb logcat`, whose CRLF lines are stored
// without the CR.
func TestStreamExecOutFallback(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	e.SetNoExecOut("FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	got := messages(d.Store)
	if len(got) != 5 || got[4] != "FAKEA line 4" {
		t.Fatalf("stored %q", got)
	}
	if len(e.CallsMatching("-s FAKEA exec-out logcat")) != 1 || len(e.CallsMatching("-s FAKEA logcat -b")) != 1 {
		t.Fatalf("calls %q", e.Calls())
	}
	for _, g := range d.Store.Meta().Gaps {
		t.Errorf("gap from the failed exec-out: %+v", g)
	}
	// the next session goes straight to adb logcat
	m.Unwant("FAKEA", "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
	startDevice(t, m, "FAKEA")
	waitFor(t, "the second stream", 10*time.Second, func() bool { return len(e.CallsMatching("-s FAKEA logcat -b")) == 2 })
	if n := len(e.CallsMatching("-s FAKEA exec-out logcat")); n != 1 {
		t.Fatalf("exec-out retried: %d", n)
	}
}

// API < 21 never tries exec-out.
func TestStreamOldAPI(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 3, "FAKEA")
	e.SetProp("FAKEA", "ro.build.version.sdk", "19")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	d.WaitCaughtUp(context.Background(), 10*time.Second)
	if len(e.CallsMatching("exec-out")) != 0 || len(e.CallsMatching("-s FAKEA logcat -b")) != 1 {
		t.Fatalf("calls %q", e.Calls())
	}
}

// One adb shell identifies the device and probes it for the recorder.
func TestMergedProbe(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 3, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	d.WaitCaughtUp(context.Background(), 10*time.Second)
	if c := e.CallsMatching("boot_id"); len(c) != 1 || !strings.Contains(c[0], "ro.serialno") {
		t.Fatalf("probes %q", c)
	}
	if mt := d.Store.Meta(); mt.Model != "Fake Phone" || mt.SDK != "29" || mt.Release != "10" || mt.TZ != "UTC" || mt.LastBootID != "boot-FAKEA" {
		t.Fatalf("meta %+v", mt)
	}
	// a restart long after the identity probe probes again (cheap variant)
	testTiming(t).ProbeReuse.Set(0)
	m.Unwant("FAKEA", "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
	startDevice(t, m, "FAKEA")
	// tracking stopped with the device, so the identity probe runs again
	// (merged), then the recorder probes by itself (cheap variant)
	waitFor(t, "the recorder probe", 10*time.Second, func() bool { return len(e.CallsMatching("boot_id")) == 3 })
	c := e.CallsMatching("boot_id")
	if !strings.Contains(c[1], "ro.serialno") || strings.Contains(c[2], "ro.serialno") || strings.Contains(c[2], "settings") {
		t.Fatalf("probes %q", c)
	}
}

func TestProbeClock(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1000, 0)
	p := &Probe{Props: map[string]string{"now": "1791684325.000000000", "now2": "1791684325.800000000"}, T0: t0, T1: t0.Add(time.Second)}
	dev, coarse, mid, ok := p.Clock()
	// 0.8 s of the 1 s round trip were spent between now and now2
	if !ok || coarse || dev.Unix() != 1791684325 || mid != t0.Add(100*time.Millisecond) {
		t.Fatalf("%v %v %v %v", dev, coarse, mid, ok)
	}
	p.Props = map[string]string{"now": "1791684325"}
	if _, coarse, mid, ok := p.Clock(); !ok || !coarse || mid != t0.Add(500*time.Millisecond) {
		t.Fatalf("coarse: %v %v %v", coarse, mid, ok)
	}
	for _, k := range []string{"ro.serialno", "settings get secure android_id", "now2="} {
		if !strings.Contains(identityCmd(), k) || (k != "ro.serialno" && strings.Contains(probeCmd(false), k)) {
			t.Errorf("%s in the probe commands", k)
		}
	}
	if strings.Index(identityCmd(), "now=") > strings.Index(identityCmd(), "settings") {
		t.Error("the clock is read after the slow settings call")
	}
}

// The progress note shows the estimated total.
func TestBackfillEstimate(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	slowBackfill(t, e, 60, 50*time.Millisecond) // ~3 s
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	var p Progress
	waitFor(t, "an estimate", 10*time.Second, func() bool { p = d.CatchUp(); return p.EstTotal > 0 || !p.Pending })
	if !p.Pending || p.EstTotal != 60 {
		t.Fatalf("progress %+v", p)
	}
	if txt := p.BackfillText(time.UTC); !strings.Contains(txt, " of ~60 lines") {
		t.Fatalf("text %q", txt)
	}
}

func TestBackfillText(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 1, 2, 3, 4e6, time.UTC).UnixNano()
	for _, c := range []struct {
		p    Progress
		want string
	}{
		{Progress{Lines: 1423, EstTotal: 26322, EarliestNs: at}, "backfill in progress (1,423 of ~26,000 lines; earliest 2026-10-11 01:02:03.004)"},
		{Progress{Lines: 12, EstTotal: 339}, "backfill in progress (12 of ~340 lines)"},
		{Progress{Lines: 500, EstTotal: 80}, "backfill in progress (500 of ~500 lines)"},
		{Progress{Lines: 1234567}, "backfill in progress (1,234,567 lines so far)"},
	} {
		if got := c.p.BackfillText(time.UTC); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	stats := `size/num main               events             system             crash              Total
Total    1869680/19624      707041/4099        406342/3738        9615/76            2992678/27537
Now      1784359/18602      705019/4098        387872/3546        9615/76            2886865/26322
Logspan  14:57:45.475       14:57:41.049       14:57:45.209       49:58.24           1d 6:47:11.139
`
	if n := parseStatsTotal(stats); n != 26322 {
		t.Fatalf("total %d", n)
	}
	if n := parseStatsTotal("logcat: unknown option -S"); n != 0 {
		t.Fatalf("total %d", n)
	}
}
