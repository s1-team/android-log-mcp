package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// Issue #41: a device process whose application uid is not in the package
// list makes the session read `pm list packages -U` again, at most once per
// packagesRefreshEvery, however many such processes start.
func TestUnknownUIDRefreshesPackagesOncePerWindow(t *testing.T) {
	t.Parallel()
	testTiming(t).PackagesRefreshEvery.Set(3 * time.Second)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	pm := func() int { return len(e.CallsMatching("pm list packages")) }
	waitFor(t, "the first package list read", 30*time.Second, func() bool { return pm() == 1 && d.sideWorkOn() })

	// new device processes reach the tracker the way psLoop's ps snapshot
	// does (the trigger is the same for am_proc_start lines)
	pid := 20000
	burst := func() {
		t.Helper()
		p := d.pipeline()
		if p == nil {
			t.Fatal("no pipeline")
		}
		var ps []procs.PSEntry
		for i := 0; i < 5; i++ {
			pid++
			ps = append(ps, procs.PSEntry{PID: pid, UID: 10990 + pid%5, Name: fmt.Sprintf("com.new.app%d", pid)})
		}
		p.Procs.Seed(ps, d.Now().UnixNano())
	}
	// within the window of the first read: no extra read
	burst()
	time.Sleep(300 * time.Millisecond)
	if n := pm(); n != 1 {
		t.Fatalf("pm list packages ran %d times within the first window, want 1", n)
	}
	// a new window: exactly one more read for two bursts of unknown uids
	time.Sleep(testTiming(t).PackagesRefreshEvery.Get())
	burst()
	burst()
	waitFor(t, "a second package list read", 30*time.Second, func() bool { return pm() == 2 })
	time.Sleep(300 * time.Millisecond)
	if n := pm(); n != 2 {
		t.Fatalf("pm list packages ran %d times, want 2 (at most once per window)", n)
	}
}
