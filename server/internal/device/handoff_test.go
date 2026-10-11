package device

import (
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #30: a quick disconnect/reconnect must not overlap two recording
// sessions of one device, and swap() closes a replaced read-only store at
// once.

// probeAfter waits for the first probe adb call logged after call n.
func probeAfter(t *testing.T, e interface {
	CallsMatching(string) []string
}, n int) {
	t.Helper()
	waitFor(t, "the new session's probe", 30*time.Second, func() bool {
		return len(e.CallsMatching("boot_id=$(cat")) > n
	})
}

func captureLog(t *testing.T) *syncBuf {
	var buf syncBuf
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func setHook(t *testing.T, f func(*Device, uint64)) {
	sessionEndHook.Store(&f)
	t.Cleanup(func() { sessionEndHook.Store(nil) })
}

// Background work of the previous session that ignores the cancellation:
// the new session's first adb call (the probe) starts only after it was
// killed and returned, and logcat_status says the new session started
// before the previous one had stopped.
func TestReconnectWaitsForPreviousBackgroundWork(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	if err := e.SetHang("FAKEA", "STUCKCMD"); err != nil {
		t.Fatal(err)
	}
	bgStopWait.Set(300 * time.Millisecond)
	t.Cleanup(func() { bgStopWait.Set(5 * time.Second) })
	buf := captureLog(t)
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	var returned atomic.Int64
	d.bg.spawn("stuck", func() {
		// not the session context: only the kill after bgStopWait ends it
		d.adb.Shell(withChildren(context.Background(), &d.kids), d.Addr(), time.Minute, "STUCKCMD")
		returned.Store(time.Now().UnixNano())
	})
	waitFor(t, "the stuck command", 30*time.Second, e.Hanging)
	probes := len(e.CallsMatching("boot_id=$(cat"))
	t0 := time.Now()
	d.setState(m.ctx, "")
	d.setState(m.ctx, "device")
	probeAfter(t, e, probes)
	var probeAt time.Time
	for _, c := range e.CallLog() {
		if strings.Contains(c.Args, "boot_id=$(cat") && c.At.After(t0) {
			probeAt = c.At
			break
		}
	}
	r := returned.Load()
	if r == 0 || probeAt.UnixNano() < r {
		t.Fatalf("the new session probed at %s, the stuck work returned at %d (0: not yet)", probeAt.Format(time.StampMicro), r)
	}
	waitFor(t, "recording again", 30*time.Second, func() bool { return d.Status().Recording })
	out := buf.String()
	for _, want := range []string{
		"FAKEA: background work did not stop within 300ms: stuck; killed its adb processes: adb -s FAKEA shell STUCKCMD",
		"FAKEA: recording session 2: starting although background work of an earlier session did not stop within 300ms: stuck",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if n := d.HandoffNote(); !strings.Contains(n, "recording session 2 started at ") || !strings.Contains(n, "before session 1 had stopped") || !strings.Contains(n, "stuck") {
		t.Errorf("HandoffNote = %q", n)
	}
	// a later clean reconnect clears the note
	d.setState(m.ctx, "")
	d.setState(m.ctx, "device")
	waitFor(t, "a clean handover", 30*time.Second, func() bool { return d.HandoffNote() == "" && d.Status().Recording })
}

// The previous session's recorder is still stopping (draining into the
// store): the new session starts only after it returned.
func TestReconnectWaitsForPreviousRecorder(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	release := make(chan struct{})
	var ended atomic.Int64
	setHook(t, func(hd *Device, sess uint64) {
		if hd == d && sess == 1 {
			<-release
			ended.Store(time.Now().UnixNano())
		}
	})
	probes := len(e.CallsMatching("boot_id=$(cat"))
	d.setState(m.ctx, "")
	d.setState(m.ctx, "device")
	time.Sleep(300 * time.Millisecond)
	if n := len(e.CallsMatching("boot_id=$(cat")); n > probes {
		t.Fatal("the new session probed while the previous recorder was still stopping")
	}
	if !d.CatchUp().Pending {
		t.Fatal("a call during the handover would not wait for the new session's catch-up")
	}
	close(release)
	probeAfter(t, e, probes)
	if ended.Load() == 0 {
		t.Fatal("probe before the previous recorder returned")
	}
	waitFor(t, "recording again", 30*time.Second, func() bool { return d.Status().Recording })
	if n := d.HandoffNote(); n != "" {
		t.Errorf("clean handover left a note: %q", n)
	}
}

// A previous recorder that does not stop within recorderStopWait: the new
// session starts anyway, and both the service log and logcat_status say
// which device and which sessions.
func TestReconnectRecorderTimeoutIsReported(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	recorderStopWait.Set(300 * time.Millisecond)
	t.Cleanup(func() { recorderStopWait.Set(10 * time.Second) })
	buf := captureLog(t)
	m, cancel := closeManager(t)
	release := make(chan struct{})
	defer func() { close(release); cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	setHook(t, func(hd *Device, sess uint64) {
		if hd == d && sess == 1 {
			<-release
		}
	})
	probes := len(e.CallsMatching("boot_id=$(cat"))
	d.setState(m.ctx, "")
	d.setState(m.ctx, "device")
	probeAfter(t, e, probes) // the new session starts although session 1 is stuck
	waitFor(t, "recording again", 30*time.Second, func() bool { return d.Status().Recording })
	if out := buf.String(); !strings.Contains(out, "FAKEA: recording session 2: the recorder of session 1 did not stop within 300ms; starting anyway") {
		t.Errorf("log:\n%s", out)
	}
	if n := d.HandoffNote(); !strings.Contains(n, "recording session 2 started at ") || !strings.Contains(n, "the recorder of session 1 did not stop within 300ms") {
		t.Errorf("HandoffNote = %q", n)
	}
}

// Rapid reconnects: the previous session never ends the new session's
// pending catch-up (it did in about 6 of 20 before #30).
func TestReconnectKeepsNewCatchUp(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	early := 0
	for i := 0; i < 20; i++ {
		d.setState(m.ctx, "")
		d.setState(m.ctx, "device")
		d.catch.mu.Lock()
		ch0, pending := d.catch.done, d.catch.pending
		d.catch.mu.Unlock()
		if !pending {
			t.Fatal("no pending catch-up right after the reconnect")
		}
		waitFor(t, "recording", 30*time.Second, func() bool { return d.Status().Recording })
		d.catch.mu.Lock()
		ch1 := d.catch.done
		d.catch.mu.Unlock()
		if ch1 != ch0 {
			early++ // ch0 was closed before the new session's recordOnce began
		}
	}
	if early > 0 {
		t.Fatalf("the new session's catch-up was ended early in %d of 20 reconnects", early)
	}
}

// swap() closes the read-only store it replaces at once (no timer); a tool
// call still holding the old device keeps reading, including what the new
// recorder writes.
func TestSwapClosesReplacedReadOnlyStore(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	m.Unwant("FAKEA", "test")
	ro := m.Get("FAKEA")
	if ro == nil || ro.Owned() || ro.Store.Closed() {
		t.Fatalf("not a read-only device after Unwant: %+v", ro)
	}
	d = startDevice(t, m, "FAKEA")
	if d == ro || !ro.Store.Closed() {
		t.Fatal("the replaced read-only store is still open")
	}
	waitFor(t, "old device still reads", 10*time.Second, func() bool {
		ro.Store.Refresh()
		return len(messages(ro.Store)) == 5
	})
}

// Not reached by attach today: if swap() ever replaces an owned device, it
// shuts it down like release() (recorder, background work, adb children)
// before closing its store.
func TestSwapOwnedDeviceShutsDown(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	if err := e.SetHang("FAKEA", "STUCKCMD"); err != nil {
		t.Fatal(err)
	}
	bgStopWait.Set(300 * time.Millisecond)
	t.Cleanup(func() { bgStopWait.Set(5 * time.Second) })
	m, cancel := closeManager(t)
	d := startDevice(t, m, "FAKEA")
	d.bg.spawn("stuck", func() {
		d.adb.Shell(withChildren(context.Background(), &d.kids), d.Addr(), time.Minute, "STUCKCMD")
	})
	waitFor(t, "the stuck command", 30*time.Second, e.Hanging)
	st, err := store.OpenReadOnly(d.Store.Dir, m.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	m.swap("FAKEA", m.newDevice(st, "FAKEA"))
	if !d.Store.Closed() || d.Status().Recording {
		t.Fatal("the owned device was not shut down")
	}
	if names := d.bg.wait(0); len(names) > 0 {
		t.Fatalf("background work still running after swap (%s): %v", time.Since(t0), names)
	}
	cancel()
	m.Close()
	waitFor(t, "fake adb processes to exit", 5*time.Second, func() bool { return len(e.Running()) == 0 })
}
