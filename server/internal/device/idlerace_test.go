package device

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #15: races between an idle stop and a new call.

// A call that arrives after CheckIdle decided to stop a device but before
// the stop takes the lock keeps the device recording: the stop re-checks
// idleness and the in-call busy count under wantMu.
func TestCallBetweenIdleCheckAndStop(t *testing.T) {
	// serial: sets the package-wide beforeIdleStop hook
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	m := newTestManager(t, t.TempDir(), store.Config{})
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	var linger atomic.Int64
	linger.Store(int64(300 * time.Millisecond))
	m.Linger = func(*Device) time.Duration { return time.Duration(linger.Load()) }
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	d := startDevice(t, m, "FAKEA")

	fired := make(chan struct{})
	var once sync.Once
	var callDone func()
	beforeIdleStop.Store(func(id string) {
		once.Do(func() {
			// a tool call: Want + Begin, still in progress when the
			// stop runs
			if err := m.Want(id, true); err != nil {
				t.Error(err)
			}
			callDone = m.Begin(id)
			linger.Store(int64(time.Minute))
			close(fired)
		})
	})
	t.Cleanup(func() { beforeIdleStop.Store(func(string) {}) })
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the idle stop never ran")
	}
	time.Sleep(300 * time.Millisecond) // a wrong release finishes by now
	defer callDone()
	if !m.Wanted("FAKEA") || m.Get("FAKEA") != d || !d.Owned() || !d.started() || !e.LogcatRunning("FAKEA") {
		t.Fatalf("the device was released under a call: wanted=%v same=%v owned=%v started=%v logcat=%v",
			m.Wanted("FAKEA"), m.Get("FAKEA") == d, d.Owned(), d.started(), e.LogcatRunning("FAKEA"))
	}
}

// A saved identity probe is used once, and its device clock is advanced
// by the time since it was taken.
func TestSavedProbeAdvancedAndUsedOnce(t *testing.T) {
	t.Parallel()
	st, err := store.OpenDir(t.TempDir(), "X", store.Config{IdleRelease: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newDevice(nil, st, "X")
	taken := time.Now().Add(-20 * time.Second)
	d.preProbe = &Probe{T0: taken, T1: taken, Props: map[string]string{
		"boot_id": "boot-X", "zone": "+0000",
		"now": fmt.Sprintf("%d.%09d", taken.Unix(), taken.Nanosecond()),
	}}
	if err := d.probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.preProbe != nil {
		t.Fatal("the saved probe was kept for another use")
	}
	got := time.Unix(0, d.devNowNs)
	if diff := time.Since(got); diff < -time.Second || diff > time.Second {
		t.Fatalf("device clock from a 20 s old probe is off by %s", diff)
	}
}

// recorderProbes counts the recorder's own probes of serial (the identity
// probe also reads android_id).
func recorderProbes(e fakeadb.Env) func(serial string) int {
	return func(serial string) int {
		n := 0
		for _, c := range e.CallsMatching("-s " + serial + " shell") {
			if strings.Contains(c, "boot_id") && !strings.Contains(c, "android_id") {
				n++
			}
		}
		return n
	}
}

// The probe saved with a device's identity serves only its first start:
// a restart while tracking goes on (the identity stays cached) probes the
// device again instead of reusing the old clock reading.
func TestSavedProbeNotReusedOnRestart(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA", "FAKEB")
	m := lazyManager(t, time.Minute)
	probes := recorderProbes(e)
	startDevice(t, m, "FAKEA")
	startDevice(t, m, "FAKEB") // keeps tracking (and A's identity) alive
	if n := probes("FAKEA"); n != 0 {
		t.Fatalf("first start: %d recorder probes, want 0 (the saved one)", n)
	}
	m.Unwant("FAKEA", "test")
	waitFor(t, "A stopped", 10*time.Second, func() bool { return !e.LogcatRunning("FAKEA") && !m.Wanted("FAKEA") })
	startDevice(t, m, "FAKEA")
	if n := probes("FAKEA"); n != 1 {
		t.Fatalf("restart: %d recorder probes, want 1 (the saved probe is used once)", n)
	}
}
