package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
)

// Issue #41: a device process whose application uid is not in the package
// list makes the session read `pm list packages -U` again, at most once per
// packagesRefreshEvery, however many such processes start.
func TestUnknownUIDRefreshesPackagesOncePerWindow(t *testing.T) {
	old := packagesRefreshEvery.Get()
	packagesRefreshEvery.Set(3 * time.Second)
	t.Cleanup(func() { packagesRefreshEvery.Set(old) })
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	pm := func() int { return len(e.CallsMatching("pm list packages")) }
	waitFor(t, "the first package list read", 30*time.Second, func() bool { return pm() == 1 && d.sideWorkOn() })

	pid := 20000
	burst := func() {
		t.Helper()
		var lines []string
		for i := 0; i < 5; i++ {
			pid++
			lines = append(lines, fakeadb.Line(time.Now(), 1500, "am_proc_start",
				fmt.Sprintf("[0,%d,%d,com.new.app%d,service,{com.new.app%d/.S}]", pid, 10990+pid%5, pid, pid)))
		}
		if err := e.Log("FAKEA", lines...); err != nil {
			t.Fatal(err)
		}
		last := pid
		waitFor(t, "the new device processes", 30*time.Second, func() bool {
			p := d.pipeline()
			return p != nil && p.Procs.Lookup(last, time.Now().Add(time.Hour).UnixNano()) != nil
		})
	}
	// within the window of the first read: no extra read
	burst()
	time.Sleep(300 * time.Millisecond)
	if n := pm(); n != 1 {
		t.Fatalf("pm list packages ran %d times within the first window, want 1", n)
	}
	// a new window: exactly one more read for two bursts of unknown uids
	time.Sleep(packagesRefreshEvery.Get())
	burst()
	burst()
	waitFor(t, "a second package list read", 30*time.Second, func() bool { return pm() == 2 })
	time.Sleep(300 * time.Millisecond)
	if n := pm(); n != 2 {
		t.Fatalf("pm list packages ran %d times, want 2 (at most once per window)", n)
	}
}
