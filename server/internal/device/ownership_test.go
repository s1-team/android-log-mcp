package device

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func testManager(t *testing.T, base, root string) *Manager {
	t.Helper()
	m := NewManager(base, store.Config{IdleRelease: -1})
	m.Root = layout.Root{Dir: root}
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

// Two processes with different ANDROID_LOG_MCP_HOME: the first owns the
// device; the second never records it, follows the owner's data and names
// the owner. When the owner goes away the second may take over.
func TestOwnershipAcrossHomes(t *testing.T) {
	root := t.TempDir()
	homeB := t.TempDir()
	id := Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"}

	m1 := testManager(t, root, root)
	m1.Attach(id)
	d1 := m1.Get("avd-a")
	if d1 == nil || !d1.Owned() || !d1.Store.IsWriter() {
		t.Fatal("first process does not own the device")
	}
	if d1.Store.Dir != layout.DeviceDir(root, "avd-a") {
		t.Fatalf("data dir %s", d1.Store.Dir)
	}
	d1.Store.Append(recs(5, time.Now().UnixNano(), "b1"))
	d1.Store.Flush()

	m2 := testManager(t, homeB, root)
	m2.Attach(id)
	d2 := m2.Get("emulator-5554") // by adb serial
	if d2 == nil {
		d2 = m2.Get("avd-a")
	}
	if d2 == nil || d2.Owned() || d2.Store.IsWriter() {
		t.Fatal("second process records a device owned elsewhere")
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
	if _, err := os.Stat(layout.DeviceDir(homeB, "avd-a")); err == nil {
		t.Fatal("second home got a data directory for the owned device")
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

	// owner exits: the other process may take over (into its own home)
	m1.Close()
	m2.Attach(id)
	d3 := m2.Get("avd-a")
	if d3 == nil || !d3.Owned() || d3.Status().Owner != nil {
		t.Fatal("no takeover after the owner exited")
	}
	if d3.Store.Dir != layout.DeviceDir(homeB, "avd-a") {
		t.Fatalf("takeover writes %s", d3.Store.Dir)
	}
	if o, held := m2.Root.ReadOwner("avd-a"); !held || o == nil || o.Home != homeB {
		t.Fatalf("owner lock after takeover: %+v %v", o, held)
	}
}

func flatDir(t *testing.T, base, serial, boot string, n int) (string, uint64, int64) {
	t.Helper()
	st, err := store.Open(base, serial, store.Config{IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour).UnixNano()
	st.UpdateMeta(func(m *store.Meta) {
		m.LastBootID = boot
		m.Sessions = append(m.Sessions, store.Session{BootID: boot, Boot: store.BootShort(boot), FirstSeq: 1})
	})
	st.Append(recs(n, t0, store.BootShort(boot)))
	st.AppendAux("crashes.jsonl", map[string]string{"id": "c1"})
	last, lastNs := st.LastSeq(), st.Meta().LastTimeNs
	st.Close()
	return filepath.Join(base, store.SanitizeSerial(serial)), last, lastNs
}

// A flat pre-v1 directory of the connecting device is moved into
// v1/<id> with all data; the old directory disappears; old names still
// resolve (cursors, device=).
func TestMigrateFlatLayout(t *testing.T) {
	root := t.TempDir()
	old, last, lastNs := flatDir(t, root, "emulator-5556", "boot-1111", 50)
	other, _, _ := flatDir(t, root, "emulator-5554", "boot-2222", 10) // another emulator's old data

	m := testManager(t, root, root)
	m.loadDirs()
	if m.Get("emulator-5556") == nil {
		t.Fatal("legacy dir not loaded")
	}
	m.Attach(Identity{ID: "avd-api29", Source: "emu_avd_name", Addr: "emulator-5556", BootID: "boot-1111"})
	d := m.Get("avd-api29")
	if d == nil || !d.Owned() {
		t.Fatal("not attached")
	}
	if d.Store.Dir != layout.DeviceDir(root, "avd-api29") {
		t.Fatalf("dir %s", d.Store.Dir)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old flat directory still there")
	}
	if d.Store.LastSeq() != last || d.Store.Meta().LastTimeNs != lastNs {
		t.Fatalf("data not preserved: last seq %d/%d, last time %d/%d", d.Store.LastSeq(), last, d.Store.Meta().LastTimeNs, lastNs)
	}
	if n := len(d.Store.ReadAux("crashes.jsonl")); n != 1 {
		t.Fatalf("aux file lost (%d lines)", n)
	}
	mt := d.Store.Meta()
	if mt.Serial != "avd-api29" || mt.Addr != "emulator-5556" || mt.IDSource != "emu_avd_name" || len(mt.Aliases) == 0 || mt.Aliases[0] != "emulator-5556" {
		t.Fatalf("meta %+v", mt)
	}
	if !d.Matches("emulator-5556") || m.Get("emulator-5556") != d {
		t.Fatal("old serial does not resolve to the migrated device")
	}
	for _, x := range m.Devices() {
		if x.Store.Dir == old {
			t.Fatal("stale entry for the moved directory")
		}
	}
	// the other emulator's data (different boot) stays as legacy data
	if _, err := os.Stat(filepath.Join(other, "meta.json")); err != nil {
		t.Fatal("unrelated flat directory was touched")
	}
	if x := m.Get("emulator-5554"); x == nil || x.Store.Dir != other {
		t.Fatal("unrelated legacy data not browsable")
	}
	// a new v1 store keeps appending after the migrated seqs
	d.Store.Append(recs(3, time.Now().UnixNano(), "x"))
	if d.Store.LastSeq() != last+3 {
		t.Fatalf("seq continuity: %d", d.Store.LastSeq())
	}
}

// Physical device: the old directory is named after ro.serialno (the USB
// serial), which is also the new ID, so it migrates even after a reboot.
func TestMigratePhysicalBySerial(t *testing.T) {
	root := t.TempDir()
	old, last, _ := flatDir(t, root, "R58M123ABCD", "boot-old", 7)
	m := testManager(t, root, root)
	m.loadDirs()
	m.Attach(Identity{ID: "R58M123ABCD", Source: "ro.serialno", Addr: "192.168.1.20:5555", BootID: "boot-new"})
	d := m.Get("R58M123ABCD")
	if d == nil || !d.Owned() || d.Store.LastSeq() != last || d.Addr() != "192.168.1.20:5555" {
		t.Fatalf("physical migration failed: %+v", d)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old directory remains")
	}
	if len(m.Devices()) != 1 {
		t.Fatalf("%d devices", len(m.Devices()))
	}
}

// No migration while an old version still records the flat directory: the
// device is followed read-only and the old process is named.
func TestLegacyWriterIsFollowed(t *testing.T) {
	root := t.TempDir()
	old, _, _ := flatDir(t, root, "emulator-5556", "boot-1", 4)
	f, ok := store.TryLock(filepath.Join(old, "writer.lock"))
	if !ok {
		t.Fatal("lock")
	}
	os.WriteFile(filepath.Join(old, "writer.lock"), []byte("4242"), 0o644)
	m := testManager(t, root, root)
	m.loadDirs()
	m.Attach(Identity{ID: "avd-api29", Source: "emu_avd_name", Addr: "emulator-5556", BootID: "boot-1"})
	d := m.Get("avd-api29")
	if d == nil || d.Owned() || d.Store.Dir != old {
		t.Fatal("legacy writer not followed")
	}
	if o := d.Status().Owner; o == nil || !o.Legacy || o.PID != 4242 {
		t.Fatalf("owner %+v", o)
	}
	if m.Root.OwnerHeld("avd-api29") {
		t.Fatal("took the owner lock while an old version records the device")
	}
	// the old version exits: next attach migrates and owns it
	store.ReleaseLock(f)
	m.Attach(Identity{ID: "avd-api29", Source: "emu_avd_name", Addr: "emulator-5556", BootID: "boot-1"})
	d = m.Get("avd-api29")
	if d == nil || !d.Owned() || d.Store.Dir != layout.DeviceDir(root, "avd-api29") || d.Store.LastSeq() != 4 {
		t.Fatal("no takeover/migration after the old version exited")
	}
}

// Startup handling of a legacy backup next to the root: moved into
// legacy/, kept while fresh, removed once older than MaxAge.
func TestLegacyBackupAdoptedAndExpired(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "android-log-mcp")
	backup := filepath.Join(parent, "android-log-mcp.v1-backup")
	flatDir(t, backup, "emulator-5556", "b", 5)
	r := layout.Root{Dir: root}
	r.Ensure()
	if moved := r.AdoptSiblings(); len(moved) != 1 {
		t.Fatalf("moved %v", moved)
	}
	m := testManager(t, root, root)
	m.sweepLocked(nil)
	if _, err := os.Stat(filepath.Join(root, "legacy", "v1-backup", "emulator-5556", "meta.json")); err != nil {
		t.Fatal("fresh legacy data removed early")
	}
	old := time.Now().Add(-48 * time.Hour)
	filepath.Walk(filepath.Join(root, "legacy"), func(p string, _ os.FileInfo, _ error) error {
		os.Chtimes(p, old, old)
		return nil
	})
	m.sweepLocked(nil)
	if _, err := os.Stat(filepath.Join(root, "legacy")); !os.IsNotExist(err) {
		t.Fatal("expired legacy backup not removed")
	}
}
