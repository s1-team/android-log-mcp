package device

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
)

// leakWait bounds waits on fake adb under a starved CI runner (Windows,
// many packages at once): spawning the fake logcat child alone can take
// more than 30 s (#222, same class as #220 / #221).
const leakWait = 60 * time.Second

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
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		st := d.Status()
		d.bg.mu.Lock()
		workers := make(map[uint64]string, len(d.bg.run))
		for k, v := range d.bg.run {
			workers[k] = v
		}
		d.bg.mu.Unlock()
		t.Logf("recording=%v state=%q owned=%v lastErr=%q workers=%v\nstream calls exec-out=%d logcat=%d\nfake adb calls (tail):\n%s",
			st.Recording, st.State, st.Owned, st.LastErr, workers,
			len(e.CallsMatching("-s FAKEA exec-out logcat -b")),
			len(e.CallsMatching("-s FAKEA logcat -b")),
			e.CallsTail(60))
	})
	for i := 1; i <= 3; i++ {
		// a stream killed before its first line is retried without exec-out, so
		// count both forms of the logcat start
		streamCount := func() int {
			return len(e.CallsMatching("-s FAKEA exec-out logcat -b")) + len(e.CallsMatching("-s FAKEA logcat -b"))
		}
		streams := streamCount()
		b, _ := os.ReadFile(filepath.Join(e.Dir, "dev", "FAKEA", "logcat"))
		// only the newest stream (the file lists every earlier one too, whose
		// pids may be reused by now)
		if f := strings.Fields(string(b)); len(f) > 0 {
			if pid, _ := strconv.Atoi(f[len(f)-1]); pid > 0 && fakeadb.Alive(pid) {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill() // adb logcat dies; the device stays attached
				}
			}
		}
		waitFor(t, "the new logcat stream after kill (restart "+strconv.Itoa(i)+")", leakWait, func() bool { return streamCount() > streams })
		waitFor(t, "recording again after kill (restart "+strconv.Itoa(i)+")", leakWait, func() bool { return d.Status().Recording })
	}
	time.Sleep(500 * time.Millisecond)
	if n := count("reconcile"); n != 1 {
		t.Errorf("%d reconcile loops running after 3 in-session restarts, want 1 (workers: %v)", n, d.bg.run)
	}
}
