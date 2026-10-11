package device

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// testManager records into base.
func testManager(t *testing.T, base string) *Manager {
	t.Helper()
	m := newTestManager(t, base, store.Config{IdleRelease: -1})
	m.ctx = context.Background()
	m.Version = "test"
	m.Root.Ensure()
	t.Cleanup(m.Close)
	return m
}

func recs(n int, t0 int64, boot string) []*model.Record {
	out := make([]*model.Record, n)
	for i := range out {
		out[i] = &model.Record{TimeNs: t0 + int64(i)*1e6, PID: 100, TID: 1, Level: 'I', Tag: "T", Msg: "m", Boot: boot, UID: -1}
	}
	return out
}

// Only the service records (one per user, its lock). A directory that
// another process still writes (writer.lock held, e.g. an older version)
// is not written: the device is attached but not recorded, and status can
// name that process.
func TestWriterLockHeldElsewhereIsNotRecorded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	id := Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"}
	other, err := store.OpenDir(layout.DeviceDir(root, "avd-a"), "avd-a", store.Config{IdleRelease: -1}, true)
	if err != nil || !other.IsWriter() {
		t.Fatal("setup writer", err)
	}
	other.Append(recs(5, time.Now().UnixNano(), "b1"))
	other.Flush()
	m := testManager(t, root)
	m.Attach(id)
	d := m.Get("emulator-5554")
	if d == nil || !d.Owned() || d.Store.IsWriter() {
		t.Fatalf("device %+v", d)
	}
	d.adb = &ADB{Path: "/nonexistent"}
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if d.Store.IsWriter() || d.Status().Recording {
		t.Fatal("recorded a directory another process writes")
	}
	// once the other writer is gone, the next attempt takes over
	other.Close()
	if !d.Store.TryPromote() {
		t.Fatal("no takeover after the other writer closed")
	}
}

// The default root (os.UserCacheDir()/android-log-mcp, redirected to a temp
// dir by TestMain) records.
func TestDefaultRootRecords(t *testing.T) {
	// serial: records into the shared default root of the test home
	m := newTestManager(t, layout.DefaultDir(), store.Config{IdleRelease: -1})
	m.ctx = context.Background()
	m.Version = "test"
	m.Root.Ensure()
	t.Cleanup(m.Close)
	m.Attach(Identity{ID: "avd-h", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"})
	d := m.Get("avd-h")
	if d == nil || !d.Owned() || !d.Store.IsWriter() || d.Store.Dir != layout.DeviceDir(layout.DefaultDir(), "avd-h") {
		t.Fatalf("default root does not record: %+v", d)
	}
	if !strings.Contains(layout.DefaultDir(), "alm-testhome-") {
		t.Fatalf("default dir %s not isolated", layout.DefaultDir())
	}
	if _, err := os.Stat(filepath.Join(layout.DefaultDir(), "owners")); !os.IsNotExist(err) {
		t.Fatal("owners/ created")
	}
}

func snapshot(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				out[p] = "missing"
				return nil
			}
			out[p] = fmt.Sprintf("%v %d %d", fi.IsDir(), fi.Size(), fi.ModTime().UnixNano())
			return nil
		})
	}
	return out
}

// --offline <dir>: browse a dataset read-only. Nothing is recorded, pruned,
// migrated or created, not even for expired data, over-cap data, old
// layouts.
func TestOfflineNeverWrites(t *testing.T) {
	t.Parallel()
	ds := filepath.Join(t.TempDir(), "store")
	st, err := store.OpenDir(layout.DeviceDir(ds, "avd-r"), "avd-r", store.Config{IdleRelease: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	st.Append(recs(20, time.Now().Add(-72*time.Hour).UnixNano(), "b"))
	st.Close()
	old := time.Now().Add(-72 * time.Hour)
	filepath.Walk(ds, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
	os.MkdirAll(filepath.Join(ds, "emulator-5554"), 0o755) // old flat layout
	os.WriteFile(filepath.Join(ds, "emulator-5554", "meta.json"), []byte(`{"serial":"emulator-5554"}`), 0o644)
	before := snapshot(t, ds)

	m := newTestManager(t, ds, store.Config{IdleRelease: -1, MaxAge: time.Hour})
	m.NoADB = true
	m.MaxTotalBytes = 1
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	d := m.Get("avd-r")
	if d == nil || d.Owned() || d.Store.IsWriter() || d.Store.LastSeq() != 20 {
		t.Fatalf("dataset not browsable: %+v", d)
	}
	if m.Get("emulator-5554") != nil {
		t.Fatal("old flat layout was read")
	}
	m.MaintainOnce(true)
	m.Attach(Identity{ID: "avd-r", Source: "avd_name", Addr: "emulator-5554", BootID: "b"})
	m.Attach(Identity{ID: "avd-new", Source: "avd_name", Addr: "emulator-5556", BootID: "c"})
	cancel()
	m.Close()
	after := snapshot(t, ds)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("offline changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// Old layouts (pre-0.4 flat <serial>/ dirs, legacy/, android-log-mcp.*
// siblings) are ignored: not read, not migrated, not deleted.
func TestOldLayoutIgnored(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "android-log-mcp")
	flat := filepath.Join(root, "emulator-5556")
	for _, dir := range []string{flat, filepath.Join(root, "legacy", "v1-backup", "emulator-5556"), filepath.Join(parent, "android-log-mcp.v1-backup", "emulator-5556")} {
		st, err := store.OpenDir(dir, "emulator-5556", store.Config{IdleRelease: -1}, true)
		if err != nil {
			t.Fatal(err)
		}
		st.Append(recs(7, time.Now().Add(-72*time.Hour).UnixNano(), "b"))
		st.Close()
	}
	old := time.Now().Add(-72 * time.Hour)
	filepath.Walk(parent, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
	before := snapshot(t, flat, filepath.Join(root, "legacy"), filepath.Join(parent, "android-log-mcp.v1-backup"))

	m := testManager(t, root)
	m.Cfg.MaxAge = time.Hour
	m.MaxTotalBytes = 1
	m.loadDirs()
	if len(m.Devices()) != 0 {
		t.Fatalf("old layout read: %d devices", len(m.Devices()))
	}
	m.sweepLocked(nil)
	m.Attach(Identity{ID: "avd-api29", Source: "emu_avd_name", Addr: "emulator-5556", BootID: "b"})
	d := m.Get("emulator-5556")
	if d == nil || !d.Owned() || d.Store.Dir != layout.DeviceDir(root, "avd-api29") || d.Store.LastSeq() != 0 {
		t.Fatalf("device not recorded fresh in v1: %+v", d)
	}
	m.MaintainOnce(true)
	if after := snapshot(t, flat, filepath.Join(root, "legacy"), filepath.Join(parent, "android-log-mcp.v1-backup")); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("old layout touched:\nbefore %v\nafter  %v", before, after)
	}
}

// Same ID on two connected devices with different boots (e.g. one AVD
// launched twice with -read-only): never one directory.
func TestIDCollisionGetsOwnDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := testManager(t, root)
	m.states = map[string]string{"emulator-5554": "device", "emulator-5556": "device"}
	first := m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A"})
	second := m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5556", BootID: "boot-B"})
	if first != "avd-x" || second != "avd-x~emulator-5556" {
		t.Fatalf("ids %q %q", first, second)
	}
	d1, d2 := m.Get("avd-x"), m.Get(second)
	if d1 == nil || d2 == nil || d1 == d2 || d1.Store.Dir == d2.Store.Dir || !d1.Owned() || !d2.Owned() {
		t.Fatal("colliding devices share a directory or are not both recorded")
	}
	if n := d2.Status().IDNote; !strings.Contains(n, "already used by another connected device (adb emulator-5554") {
		t.Fatalf("note %q", n)
	}
	if d1.Status().IDNote != "" {
		t.Fatal("first device got a note")
	}
	// re-attaching either keeps its directory
	if again := m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A"}); again != "avd-x" {
		t.Fatalf("first re-attach -> %q", again)
	}
}

// The same device over USB and Wi-Fi at once (same boot) is one device.
func TestSameDeviceTwoTransportsIsNotACollision(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := testManager(t, root)
	m.states = map[string]string{"R58M123ABCD": "device", "192.168.1.20:5555": "device"}
	a := m.Attach(Identity{ID: "R58M123ABCD", Source: "ro.serialno", Addr: "R58M123ABCD", BootID: "boot-1"})
	b := m.Attach(Identity{ID: "R58M123ABCD", Source: "ro.serialno", Addr: "192.168.1.20:5555", BootID: "boot-1"})
	if a != b || len(m.Devices()) != 1 {
		t.Fatalf("ids %q %q, %d devices", a, b, len(m.Devices()))
	}
}

// A reboot (new boot id) or a reconnect on another port after the old
// address went away is the same device, not a collision.
func TestRebootOrReconnectIsNotACollision(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := testManager(t, root)
	m.states = map[string]string{"emulator-5554": "device"}
	m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A"})
	if id := m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A2"}); id != "avd-x" {
		t.Fatalf("reboot -> %q", id)
	}
	m.states = map[string]string{"emulator-5560": "device"} // 5554 gone
	if id := m.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5560", BootID: "boot-B"}); id != "avd-x" {
		t.Fatalf("reconnect -> %q", id)
	}
	if len(m.Devices()) != 1 {
		t.Fatalf("%d devices", len(m.Devices()))
	}
}
