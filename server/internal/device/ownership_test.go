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

// testManager records into base; owners is the shared owners directory
// (the fixed per-user one in production, a temp dir here).
func testManager(t *testing.T, base, owners string) *Manager {
	t.Helper()
	m := NewManager(base, store.Config{IdleRelease: -1})
	m.Root = layout.Root{Dir: base, OwnersDir: owners}
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

// Two processes with different data roots (e.g. one started before a
// set-root, one after) share the fixed owners dir: never both record a
// device. The second follows the owner's data dir read-only and names the
// owner; once the owner exits it takes over into its own root.
func TestTwoRootsNeverBothRecord(t *testing.T) {
	owners := filepath.Join(t.TempDir(), "owners")
	rootA := t.TempDir()
	rootB := t.TempDir()
	id := Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"}

	m1 := testManager(t, rootA, owners)
	m1.Attach(id)
	d1 := m1.Get("avd-a")
	if d1 == nil || !d1.Owned() || !d1.Store.IsWriter() {
		t.Fatal("first process does not own the device")
	}
	if d1.Store.Dir != layout.DeviceDir(rootA, "avd-a") {
		t.Fatalf("data dir %s", d1.Store.Dir)
	}
	d1.Store.Append(recs(5, time.Now().UnixNano(), "b1"))
	d1.Store.Flush()

	m2 := testManager(t, rootB, owners)
	m2.Attach(id)
	d2 := m2.Get("emulator-5554") // by adb serial
	if d2 == nil || d2.Owned() || d2.Store.IsWriter() {
		t.Fatal("second root records a device owned elsewhere")
	}
	st := d2.Status()
	if st.Owner == nil || st.Owner.PID != os.Getpid() || st.Owner.DataDir != d1.Store.Dir || st.Owner.Version != "test" {
		t.Fatalf("owner %+v", st.Owner)
	}
	if d2.Store.Dir != d1.Store.Dir {
		t.Fatalf("follower reads %s, owner writes %s", d2.Store.Dir, d1.Store.Dir)
	}
	d2.Store.Refresh()
	if d2.Store.LastSeq() != 5 {
		t.Fatalf("follower sees %d records", d2.Store.LastSeq())
	}
	if _, err := os.Stat(layout.DeviceDir(rootB, "avd-a")); err == nil {
		t.Fatal("second root got a data directory for the owned device")
	}
	// recording loop of the follower never promotes
	d2.adb = &ADB{Path: "/nonexistent"}
	ctx, cancel := context.WithCancel(context.Background())
	go d2.run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if d2.Store.IsWriter() || d2.Status().Recording {
		t.Fatal("follower started recording")
	}

	// owner exits: the other root takes over, into its own root
	m1.Close()
	m2.Attach(id)
	d3 := m2.Get("avd-a")
	if d3 == nil || !d3.Owned() || !d3.Store.IsWriter() || d3.Store.Dir != layout.DeviceDir(rootB, "avd-a") {
		t.Fatalf("no takeover into the second root: %+v", d3)
	}
	o, held := m2.Root.ReadOwner("avd-a")
	if !held || o.DataDir != d3.Store.Dir || o.Root != rootB {
		t.Fatalf("owner lock %+v held %v", o, held)
	}
	// and now the first root (restarted) follows the second
	m1b := testManager(t, rootA, owners)
	m1b.Attach(id)
	if d := m1b.Get("avd-a"); d == nil || d.Owned() || d.Store.Dir != d3.Store.Dir {
		t.Fatal("restarted first root does not follow the new owner")
	}
}

// The root configured in config.json (set-root) records normally; the owner
// lock still lives in the fixed per-user owners dir (redirected to a temp
// dir by TestMain) and names the root.
func TestConfiguredRootRecords(t *testing.T) {
	home := filepath.Join(t.TempDir(), "configured")
	if r, err := layout.ConfiguredRoot(); err != nil || r != layout.DefaultDir() {
		t.Fatalf("default root %q %v", r, err)
	}
	if err := layout.WriteConfig(layout.Config{Root: home}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(layout.ConfigPath()) })
	base, err := layout.ConfiguredRoot()
	if err != nil || base != home {
		t.Fatalf("configured root %q %v", base, err)
	}
	m := NewManager(base, store.Config{IdleRelease: -1})
	if m.Root.Dir != home || m.Root.Owners() != layout.DefaultOwnersDir() {
		t.Fatalf("root %+v", m.Root)
	}
	m.ctx = context.Background()
	m.Version = "test"
	m.FollowConfig = true
	m.Root.Ensure()
	t.Cleanup(m.Close)
	m.Attach(Identity{ID: "avd-h", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"})
	d := m.Get("avd-h")
	if d == nil || !d.Owned() || !d.Store.IsWriter() || d.Store.Dir != layout.DeviceDir(home, "avd-h") {
		t.Fatalf("configured root does not record: %+v", d)
	}
	if !strings.HasPrefix(layout.DefaultOwnersDir(), os.Getenv("HOME")) || !strings.Contains(os.Getenv("HOME"), "alm-testhome-") {
		t.Fatalf("owners dir %s not isolated", layout.DefaultOwnersDir())
	}
	if !strings.Contains(layout.ConfigPath(), "alm-testhome-") {
		t.Fatalf("config %s not isolated", layout.ConfigPath())
	}
	if _, err := os.Stat(filepath.Join(layout.DefaultOwnersDir(), "avd-h.lock")); err != nil {
		t.Fatalf("owner lock not in the fixed owners dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "owners")); err == nil {
		t.Fatal("owners/ created under the configured root")
	}
	o, _ := m.Root.ReadOwner("avd-h")
	if o == nil || o.DataDir != d.Store.Dir || o.Root != home {
		t.Fatalf("owner %+v", o)
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
// layouts or the owners dir.
func TestOfflineNeverWrites(t *testing.T) {
	ds := filepath.Join(t.TempDir(), "store")
	owners := filepath.Join(t.TempDir(), "owners")
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
	before := snapshot(t, ds, owners)

	m := NewManager(ds, store.Config{IdleRelease: -1, MaxAge: time.Hour})
	m.Root.OwnersDir = owners
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
	after := snapshot(t, ds, owners)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("offline changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// Old layouts (pre-0.4 flat <serial>/ dirs, legacy/, android-log-mcp.*
// siblings) are ignored: not read, not migrated, not deleted.
func TestOldLayoutIgnored(t *testing.T) {
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

	m := testManager(t, root, filepath.Join(t.TempDir(), "owners"))
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
	root := t.TempDir()
	m := testManager(t, root, filepath.Join(root, "owners"))
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
	root := t.TempDir()
	m := testManager(t, root, filepath.Join(root, "owners"))
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
	root := t.TempDir()
	m := testManager(t, root, filepath.Join(root, "owners"))
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

// Across processes: the owner lock carries the boot id, so a second process
// seeing another instance of the same AVD files it separately instead of
// following the first instance's data.
func TestIDCollisionAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	m1 := testManager(t, root, filepath.Join(root, "owners"))
	m1.states = map[string]string{"emulator-5554": "device"}
	m1.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A"})
	if o, _ := m1.Root.ReadOwner("avd-x"); o == nil || o.BootID != "boot-A" {
		t.Fatalf("owner info %+v", o)
	}
	m2 := testManager(t, root, filepath.Join(root, "owners"))
	m2.states = map[string]string{"emulator-5554": "device", "emulator-5556": "device"}
	if id := m2.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5556", BootID: "boot-B"}); id != "avd-x~emulator-5556" {
		t.Fatalf("second instance -> %q", id)
	}
	d := m2.Get("avd-x~emulator-5556")
	if d == nil || !d.Owned() || d.Store.Dir != layout.DeviceDir(root, "avd-x~emulator-5556") {
		t.Fatal("second instance not recorded in its own directory")
	}
	// the first instance, seen from m2, is followed (same boot): no collision
	if id := m2.Attach(Identity{ID: "avd-x", Source: "avd_name", Addr: "emulator-5554", BootID: "boot-A"}); id != "avd-x" {
		t.Fatalf("first instance from m2 -> %q", id)
	}
	if f := m2.Get("avd-x"); f == nil || f.Owned() || f.Status().Owner == nil {
		t.Fatal("first instance not followed in m2")
	}
}
