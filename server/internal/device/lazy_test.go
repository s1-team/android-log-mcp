package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Lazy recording (issue #11) against a scriptable fake adb.

func fakeEnv(t *testing.T) fakeadb.Env {
	t.Helper()
	dir := t.TempDir()
	adb, err := fakeadb.Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANDROID_LOG_MCP_ADB", adb)
	t.Setenv(fakeadb.EnvDir, dir)
	return fakeadb.Env{Dir: dir}
}

// addDevices creates online devices with n buffered lines each (oldest
// first, ending ~now).
func addDevices(t *testing.T, e fakeadb.Env, n int, serials ...string) {
	t.Helper()
	states := map[string]string{}
	for _, s := range serials {
		if err := e.AddDevice(s, "boot-"+s); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		var lines []string
		for i := 0; i < n; i++ {
			lines = append(lines, fakeadb.Line(now.Add(time.Duration(i-n)*10*time.Millisecond), 100, "PRE", fmt.Sprintf("%s line %d", s, i)))
		}
		if err := e.Log(s, lines...); err != nil {
			t.Fatal(err)
		}
		states[s] = "device"
	}
	if err := e.SetDevices(states); err != nil {
		t.Fatal(err)
	}
}

func lazyManager(t *testing.T, linger time.Duration) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), store.Config{})
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	m.Linger = func(*Device) time.Duration { return linger }
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	return m
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func messages(st *store.Store) []string {
	var out []string
	st.Scan(0, st.LastSeq()+1, false, func(r *model.Record) bool {
		out = append(out, r.Msg)
		return true
	})
	return out
}

func startDevice(t *testing.T, m *Manager, id string) *Device {
	t.Helper()
	m.Track(5 * time.Second)
	if err := m.Want(id, true); err != nil {
		t.Fatal(err)
	}
	defer m.Begin(id)() // like a tool call: busy until it returns
	var d *Device
	waitFor(t, id+" recording", 30*time.Second, func() bool {
		d = m.Get(id)
		return d != nil && d.Owned() && d.Status().Recording
	})
	return d
}

// lastStream waits until the fake logged more than n logcat streams for
// serial (the process logs itself when it starts) and returns them.
func lastStream(t *testing.T, e fakeadb.Env, serial string, n int) []string {
	t.Helper()
	var calls []string
	waitFor(t, "the new logcat stream", 30*time.Second, func() bool {
		calls = e.CallsMatching("-s " + serial + " exec-out logcat -b")
		return len(calls) > n
	})
	return calls
}

func TestLazyIdleRunsNoADB(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	time.Sleep(300 * time.Millisecond)
	if c := e.Calls(); len(c) != 0 {
		t.Fatalf("an idle manager ran adb: %q", c)
	}
	if dirs := store.ListDeviceDirs(m.Base); len(dirs) != 0 {
		t.Fatalf("device directories created: %v", dirs)
	}
	if !m.Idle() || m.Tracking() {
		t.Fatal("not idle")
	}
}

// Only the wanted device is recorded; starting is safe to repeat and to
// do concurrently; the other online device stays unrecorded (no store,
// no logcat).
func TestLazyStartsOnlyWantedDevice(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA", "FAKEB")
	m := lazyManager(t, time.Minute)
	m.Track(5 * time.Second)
	if ids := m.OnlineIDs(); len(ids) != 2 {
		t.Fatalf("online %v", ids)
	}
	if len(e.CallsMatching("logcat")) != 0 {
		t.Fatal("logcat before any device was wanted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Want("FAKEA", true) }()
	}
	wg.Wait()
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return !d.WaitCaughtUp(context.Background(), time.Second).Pending })
	if got := messages(d.Store); len(got) != 5 || got[0] != "FAKEA line 0" {
		t.Fatalf("stored %q", got)
	}
	if n := len(e.CallsMatching("-s FAKEA exec-out logcat")); n != 1 {
		t.Fatalf("%d logcat streams for FAKEA: %q", n, e.Calls())
	}
	if len(e.CallsMatching("-s FAKEB exec-out logcat")) != 0 || e.LogcatRunning("FAKEB") {
		t.Fatal("FAKEB recorded")
	}
	if b := m.Get("FAKEB"); b != nil && b.Owned() {
		t.Fatal("FAKEB owned")
	}
	for _, dir := range store.ListDeviceDirs(m.Base) {
		if strings.Contains(dir, "FAKEB") {
			t.Fatalf("FAKEB directory created: %s", dir)
		}
	}
}

// Catch-up is measured (T_end), not guessed: while the buffer streams
// slowly the device is not caught up; it is right after the line at T_end
// is stored.
func TestCatchUpDeterministic(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 30, "FAKEA")
	e.SetDelay("FAKEA", 60*time.Millisecond) // ~1.8 s for the dump
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	p := d.WaitCaughtUp(context.Background(), 300*time.Millisecond)
	if !p.Pending || p.Lines >= 30 {
		t.Fatalf("caught up early: %+v", p)
	}
	if txt := p.BackfillText(time.UTC); !strings.HasPrefix(txt, "backfill in progress (") {
		t.Fatalf("text %q", txt)
	}
	p = d.WaitCaughtUp(context.Background(), 10*time.Second)
	if p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	got := messages(d.Store)
	if len(got) != 30 || got[29] != "FAKEA line 29" {
		t.Fatalf("caught up before the line at T_end was stored: %d lines", len(got))
	}
	if len(e.CallsMatching("logcat -d -t 50 -b main,system,crash,events")) == 0 {
		t.Fatal("T_end was not measured")
	}
}

func TestCatchUpEmptyBuffer(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 0, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 2*time.Second); p.Pending {
		t.Fatalf("empty buffer not caught up: %+v", p)
	}
}

// A restart of a device (idle stop, then a call) resumes with -T; with
// nothing new in the buffer it is caught up at once, and lines logged
// while it was idle are backfilled without duplicates.
func TestResumeAfterIdle(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	m.Unwant("FAKEA", "test")
	if m.Tracking() || e.LogcatRunning("FAKEA") || m.Get("FAKEA").Owned() {
		t.Fatal("still recording after Unwant")
	}
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
	// nothing new: caught up at once
	streams := len(e.CallsMatching("-s FAKEA exec-out logcat -b"))
	d = startDevice(t, m, "FAKEA")
	t0 := time.Now()
	if p := d.WaitCaughtUp(context.Background(), 5*time.Second); p.Pending || time.Since(t0) > 2*time.Second {
		t.Fatalf("resume with nothing new: %+v after %s", p, time.Since(t0))
	}
	calls := lastStream(t, e, "FAKEA", streams)
	if last := calls[len(calls)-1]; !strings.Contains(last, " -T ") {
		t.Fatalf("resume without -T: %s", last)
	}
	m.Unwant("FAKEA", "test")
	e.Log("FAKEA", fakeadb.Line(time.Now(), 100, "IDLE", "logged while idle"))
	d = startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	got := messages(d.Store)
	if len(got) != 6 || got[5] != "logged while idle" {
		t.Fatalf("stored %q", got)
	}
	for _, g := range d.Store.Meta().Gaps {
		t.Errorf("unexpected gap (the buffer still overlapped): %+v", g)
	}
}

// The device clock went back on the same boot: -T <last stored time>
// would wait for the future, so the whole buffer is read without -T and
// lines already stored are dropped.
func TestClockRollback(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	m.Unwant("FAKEA", "test")
	back := time.Now().Add(-time.Hour)
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back.Add(time.Second), 100, "AFTER", "after the rollback"))
	streams := len(e.CallsMatching("-s FAKEA exec-out logcat -b"))
	d = startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	calls := lastStream(t, e, "FAKEA", streams)
	if last := calls[len(calls)-1]; strings.Contains(last, " -T ") {
		t.Fatalf("resumed with -T after a clock rollback: %s", last)
	}
	got := messages(d.Store)
	if len(got) != 6 || got[5] != "after the rollback" {
		t.Fatalf("stored %q", got)
	}
	if note := d.RollbackNote(); !strings.Contains(note, "device clock went back") || !strings.Contains(note, "skipped 5 already stored lines") {
		t.Fatalf("rollback note %q", note)
	}
}

// Per-device idle: B stops after its linger while A, kept busy (a long
// poll), continues; with nothing recorded adb tracking stops too.
func TestPerDeviceIdle(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 3, "FAKEA", "FAKEB")
	m := lazyManager(t, 400*time.Millisecond)
	m.Track(30 * time.Second)
	if err := m.Want("FAKEA", true); err != nil {
		t.Fatal(err)
	}
	done := m.Begin("FAKEA") // e.g. logcat_search(wait_ms) in flight
	startDevice(t, m, "FAKEA")
	startDevice(t, m, "FAKEB")
	waitFor(t, "B to stop", 5*time.Second, func() bool { return !m.Wanted("FAKEB") })
	waitFor(t, "B's logcat to exit", 5*time.Second, func() bool { return !e.LogcatRunning("FAKEB") })
	waitFor(t, "B released (read-only)", 5*time.Second, func() bool {
		b := m.Get("FAKEB")
		return b != nil && !b.Owned() && !b.Store.IsWriter()
	})
	time.Sleep(600 * time.Millisecond)
	if !m.Wanted("FAKEA") || !e.LogcatRunning("FAKEA") {
		t.Fatal("A stopped while a call was in flight")
	}
	done()
	waitFor(t, "A to stop", 5*time.Second, func() bool { return !m.Wanted("FAKEA") })
	waitFor(t, "idle", 5*time.Second, func() bool { return m.Idle() && !e.AnyChildRunning() })
}

// A pin keeps a device recording past its linger.
func TestPinnedDeviceStays(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 3, "FAKEA")
	m := lazyManager(t, 100*time.Millisecond)
	var mu sync.Mutex
	until := time.Now().Add(time.Hour)
	m.PinnedUntil = func(string) time.Time { mu.Lock(); defer mu.Unlock(); return until }
	startDevice(t, m, "FAKEA")
	mu.Lock()
	until = time.Now().Add(800 * time.Millisecond)
	mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	if !m.Wanted("FAKEA") {
		t.Fatal("stopped while pinned")
	}
	waitFor(t, "stop after the pin", 5*time.Second, func() bool { return !m.Wanted("FAKEA") })
}

func TestMaxDevices(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 1, "FAKEA", "FAKEB")
	m := lazyManager(t, time.Minute)
	m.MaxDevices = 1
	startDevice(t, m, "FAKEA")
	err := m.Want("FAKEB", true)
	var me *ErrMaxDevices
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "ANDROID_LOG_MCP_MAX_DEVICES=1") {
		t.Fatalf("err %v", err)
	}
}

// Snapshot/Restore carry started devices and their timers (set-root).
func TestSnapshotRestore(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 1, "FAKEA")
	m := lazyManager(t, time.Minute)
	startDevice(t, m, "FAKEA")
	last := m.LastCall("FAKEA")
	snap := m.Snapshot()
	m2 := lazyManager(t, time.Minute)
	m2.Restore(snap)
	if !m2.Wanted("FAKEA") || !m2.LastCall("FAKEA").Equal(last) {
		t.Fatalf("restore: wanted %v last %v", m2.Wanted("FAKEA"), m2.LastCall("FAKEA"))
	}
}

func TestParseDate(t *testing.T) {
	for _, c := range []struct {
		in     string
		coarse bool
		ok     bool
	}{{"1791572000.123456789", false, true}, {"1791572000", true, true}, {"1791572000.N", true, true}, {"x", false, false}} {
		_, coarse, ok := parseDate(c.in)
		if coarse != c.coarse || ok != c.ok {
			t.Errorf("parseDate(%q) = %v %v", c.in, coarse, ok)
		}
	}
}

// Tracking started by a call that recorded nothing (here: two devices,
// none named) stops after TrackGrace: the manager is idle again and no
// adb child runs.
func TestTrackingWithoutDeviceStops(t *testing.T) {
	old := TrackGrace
	TrackGrace = 300 * time.Millisecond
	t.Cleanup(func() { TrackGrace = old })
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA", "FAKEB")
	m := lazyManager(t, time.Minute)
	m.Track(0)
	if !m.Tracking() {
		t.Fatal("not tracking")
	}
	waitFor(t, "tracking stops", 10*time.Second, func() bool { return m.Idle() && !m.Tracking() })
	waitFor(t, "no adb child", 10*time.Second, func() bool { return !e.AnyChildRunning() })
}

// After a resume (-T, de-duplication against the last stored line), the
// device clock is set back while live: the new lines, dated before the last
// stored one, are kept (de-duplication is for the replayed part only) and
// the set back is recorded as a live clock jump at the first of them
// (issue #48).
func TestSetBackWhileLiveAfterResume(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	m.Unwant("FAKEA", "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
	d = startDevice(t, m, "FAKEA")
	if p := d.WaitCaughtUp(context.Background(), 5*time.Second); p.Pending {
		t.Fatalf("resume: %+v", p)
	}
	e.Log("FAKEA", fakeadb.Line(time.Now(), 100, "LIVE", "live before the set back"))
	waitFor(t, "live line", 5*time.Second, func() bool { return len(messages(d.Store)) == 6 })
	back := time.Now().Add(-time.Hour)
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back, 100, "LIVE", "after set back 1"))
	e.Log("FAKEA", fakeadb.Line(back.Add(time.Millisecond), 100, "LIVE", "after set back 2"))
	waitFor(t, "lines after the set back", 5*time.Second, func() bool { return len(messages(d.Store)) == 8 })
	waitFor(t, "live clock jump", 5*time.Second, func() bool { return len(d.Store.ClockJumps()) == 1 })
	j := d.Store.ClockJumps()[0]
	if j.Source != "live" || j.Seq != 7 || j.DeltaNs > -int64(59*time.Minute) {
		t.Fatalf("jump %+v", j)
	}
	if off := d.Clock().Offset; off > -59*time.Minute {
		t.Fatalf("device now not moved back: offset %v", off)
	}
}
