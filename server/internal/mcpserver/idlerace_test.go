package mcpserver

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
)

// Issue #15: calls that arrive while an idle stop is releasing the device
// (device.ReleasePause holds the release open, as a slow adb on Windows
// did) re-attach it and wait for it, and never answer silently from the
// read-only device: they see the new line or the backfill note, and
// logcat_status returns no cursor before the catch-up.
func TestCallDuringIdleStop(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 3, "FAKEA")
	if out, isErr := l.call(t, "logcat_status", `{"device":"FAKEA"}`); isErr || !strings.Contains(out, "cursor (newest line") {
		t.Fatalf("first status:\n%s", out)
	}
	l.m.Timing.ReleasePause.Set(1500 * time.Millisecond)
	l.linger.Store(int64(200 * time.Millisecond))
	waitUntil(t, "the idle stop", 10*time.Second, func() bool { return !l.e.LogcatRunning("FAKEA") })
	l.linger.Store(int64(time.Minute))
	// the release is paused now: the device still looks online
	if err := l.e.Log("FAKEA", fakeadb.Line(time.Now(), 100, "RACE", "logged during the idle stop")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var search, status string
	var searchErr, statusErr bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		search, searchErr = l.call(t, "logcat_search", `{"device":"FAKEA","filter":"tag=:RACE"}`)
	}()
	go func() {
		defer wg.Done()
		status, statusErr = l.call(t, "logcat_status", `{"device":"FAKEA"}`)
	}()
	wg.Wait()
	t.Logf("search during the stop:\n%s\nstatus during the stop:\n%s", search, status)
	if searchErr || !(strings.Contains(search, "logged during the idle stop") || strings.Contains(search, "backfill in progress")) {
		t.Errorf("search during the idle stop answered without the new line or a backfill note:\n%s", search)
	}
	if statusErr {
		t.Errorf("status: %s", status)
	} else if !strings.Contains(status, "cursor (newest line") && !strings.Contains(status, "no cursor until the backfill") {
		t.Errorf("status during the idle stop:\n%s", status)
	}
	waitUntil(t, "owned and recording", 30*time.Second, func() bool {
		d := l.m.Get("FAKEA")
		return l.m.Wanted("FAKEA") && d != nil && d.Owned() && d.Status().Recording && l.e.LogcatRunning("FAKEA")
	})
	var out string
	for deadline := time.Now().Add(30 * time.Second); ; {
		out, _ = l.call(t, "logcat_search", `{"device":"FAKEA","filter":"tag=:RACE"}`)
		if !strings.Contains(out, "backfill in progress") || time.Now().After(deadline) {
			break // (a slow fake adb, e.g. on Windows, may still be catching up)
		}
	}
	if !strings.Contains(out, "logged during the idle stop") || strings.Contains(out, "backfill in progress") {
		t.Errorf("search after the restart:\n%s", out)
	}
	// a cursor returned during the stop must be the caught-up one (nothing
	// was logged since): an older one would skip the new line
	if c := cursorOf(status); c != "" {
		final, _ := l.call(t, "logcat_status", `{"device":"FAKEA"}`)
		if fc := cursorOf(final); c != fc {
			t.Errorf("status during the idle stop returned cursor %q before the catch-up (caught up: %q)", c, fc)
		}
	}
}

func cursorOf(status string) string {
	for _, l := range strings.Split(status, "\n") {
		if _, c, ok := strings.Cut(l, "cursor (newest line, FAKEA): "); ok {
			return strings.TrimSpace(c)
		}
	}
	return ""
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
