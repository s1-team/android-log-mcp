package device

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Issue #194 (promoted from the blind-device probe). It kills the fake adb logcat child
// with os.Process.Kill, which works on every OS.
// Every in-session restart of the recorder (logcat stream dies, device stays "device")
// calls recordOnce again with the SAME session ctx; recordOnce spawns sideWork + reconcileLoop
// bound to that ctx, and nothing cancels the previous iteration's ones.
func TestRecordOnceRestartDoesNotLeakWorkers(t *testing.T) {
	// serial: touches the process-wide log output? no, but kills fake children
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	testTiming(t).ReconcileEvery.Set(50 * time.Millisecond)
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	count := func(name string) int {
		d.bg.mu.Lock()
		defer d.bg.mu.Unlock()
		n := 0
		for _, v := range d.bg.run {
			if v == name {
				n++
			}
		}
		return n
	}
	for i := 1; i <= 3; i++ {
		// a stream killed before its first line is retried without exec-out, so
		// count both forms of the logcat start
		streamCount := func() int {
			return len(e.CallsMatching("-s FAKEA exec-out logcat -b")) + len(e.CallsMatching("-s FAKEA logcat -b"))
		}
		streams := streamCount()
		b, _ := os.ReadFile(filepath.Join(e.Dir, "dev", "FAKEA", "logcat"))
		for _, f := range strings.Fields(string(b)) {
			if pid, _ := strconv.Atoi(f); pid > 0 {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill() // adb logcat dies; the device stays attached
				}
			}
		}
		waitFor(t, "the new logcat stream", 30*time.Second, func() bool { return streamCount() > streams })
		waitFor(t, "recording again", 30*time.Second, func() bool { return d.Status().Recording })
	}
	time.Sleep(500 * time.Millisecond)
	if n := count("reconcile"); n != 1 {
		t.Errorf("%d reconcile loops running after 3 in-session restarts, want 1 (workers: %v)", n, d.bg.run)
	}
}
