package report

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// A report run with another data root (ANDROID_LOG_MCP_HOME) reads a device
// from the live owner's data dir: one device, one directory across roots.
func TestReportReadsLiveOwnersDataDir(t *testing.T) {
	rootA, rootB, owners := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "owners")
	dir := layout.DeviceDir(rootA, "avd-a")
	st, err := store.OpenDir(dir, "avd-a", store.Config{IdleRelease: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) { m.Addr = "emulator-5554" })
	st.Close()
	l, _, err := layout.Root{Dir: rootA, OwnersDir: owners}.AcquireOwner("avd-a", layout.Owner{PID: 1, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selectDevices(Options{Base: rootB}); err == nil {
		t.Fatal("found a device without looking at owners")
	}
	ds, err := selectDevices(Options{Base: rootB, OwnersDir: owners, Serials: []string{"emulator-5554"}, Now: time.Now})
	if err != nil || len(ds) != 1 || ds[0].serial != "avd-a" || ds[0].st.Dir != dir {
		t.Fatalf("devices %+v err %v", ds, err)
	}
	l.Release()
	if _, err := selectDevices(Options{Base: rootB, OwnersDir: owners}); err == nil {
		t.Fatal("read a device whose owner is gone from another root")
	}
}
