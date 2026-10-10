package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func write(t *testing.T, p string, size int, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		os.Chtimes(p, mod, mod)
	}
}

// devDir creates a device directory with meta.json and n segments of size
// bytes each, all last modified at mod (segment i at mod + i s).
func devDir(t *testing.T, dir string, n, size int, mod time.Time) {
	t.Helper()
	b, _ := json.Marshal(store.Meta{Serial: filepath.Base(dir)})
	write(t, filepath.Join(dir, "meta.json"), 0, mod)
	os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
	os.Chtimes(filepath.Join(dir, "meta.json"), mod, mod)
	write(t, filepath.Join(dir, "crashes.jsonl"), 10, mod)
	for i := 0; i < n; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("seg-%020d.jsonl", 1+i*100)), size, mod.Add(time.Duration(i)*time.Second))
	}
}

func TestOwnerLockExclusive(t *testing.T) {
	r := Root{Dir: t.TempDir()}
	l, _, err := r.AcquireOwner("avd-a", Owner{PID: 11, Version: "0.4.0", DataDir: "/x/v1/avd-a", Addr: "emulator-5554"})
	if err != nil {
		t.Fatal(err)
	}
	if _, o, err := r.AcquireOwner("avd-a", Owner{PID: 22}); !errors.Is(err, ErrOwned) || o == nil || o.PID != 11 || o.DataDir != "/x/v1/avd-a" {
		t.Fatalf("second acquire: err %v owner %+v", err, o)
	}
	if o, held := r.ReadOwner("avd-a"); !held || o == nil || o.Version != "0.4.0" {
		t.Fatalf("ReadOwner: %+v %v", o, held)
	}
	l.Update(func(o *Owner) { o.Addr = "127.0.0.1:5555" })
	if o, _ := r.ReadOwner("avd-a"); o == nil || o.Addr != "127.0.0.1:5555" {
		t.Fatalf("after Update: %+v", o)
	}
	// another device is independent
	l2, _, err := r.AcquireOwner("R58M", Owner{PID: 33})
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
	l.Release()
	if r.OwnerHeld("avd-a") {
		t.Fatal("still held after Release")
	}
	l3, _, err := r.AcquireOwner("avd-a", Owner{PID: 44})
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	l3.Release()
}

func TestAdoptSiblings(t *testing.T) {
	parent := t.TempDir()
	r := Root{Dir: filepath.Join(parent, "android-log-mcp")}
	os.MkdirAll(r.Dir, 0o755)
	devDir(t, filepath.Join(parent, "android-log-mcp.v1-backup", "emulator-5556"), 2, 100, time.Now())
	devDir(t, filepath.Join(parent, "android-log-mcp.busy", "emulator-5554"), 1, 100, time.Now())
	devDir(t, filepath.Join(parent, "other-tool.v1", "x"), 1, 100, time.Now())
	f, ok := store.TryLock(filepath.Join(parent, "android-log-mcp.busy", "emulator-5554", "writer.lock"))
	if !ok {
		t.Fatal("lock")
	}
	defer store.ReleaseLock(f)
	moved := r.AdoptSiblings()
	if len(moved) != 1 || moved[0] != filepath.Join(r.Legacy(), "v1-backup") {
		t.Fatalf("moved %v", moved)
	}
	if !exists(filepath.Join(r.Legacy(), "v1-backup", "emulator-5556", "meta.json")) || exists(filepath.Join(parent, "android-log-mcp.v1-backup")) {
		t.Fatal("v1-backup not moved into legacy/")
	}
	if !exists(filepath.Join(parent, "android-log-mcp.busy")) {
		t.Fatal("a sibling with a live writer was moved")
	}
	if !exists(filepath.Join(parent, "other-tool.v1")) {
		t.Fatal("unrelated directory moved")
	}
}

// Retention applies to every format, legacy data and tmp/; what this
// process records is left to its store, and unknown files are never touched.
func TestSweepMaxAgeAcrossVersions(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-time.Hour)
	devDir(t, filepath.Join(base, "v1", "avd-old"), 2, 100, old)   // current format, unowned, expired
	devDir(t, filepath.Join(base, "v1", "avd-mixed"), 2, 100, old) // one expired segment + fresh aux
	write(t, filepath.Join(base, "v1", "avd-mixed", "seg-00000000000000009999.jsonl"), 100, fresh)
	devDir(t, filepath.Join(base, "v1", "avd-owned"), 2, 100, old) // recorded by us: untouched
	devDir(t, filepath.Join(base, "v0", "dev"), 2, 100, old)       // older format
	devDir(t, filepath.Join(base, "emulator-5554"), 2, 100, old)   // flat pre-v1
	devDir(t, filepath.Join(base, "emulator-5556"), 2, 100, fresh) // flat, still fresh
	devDir(t, filepath.Join(base, "legacy", "v1-backup", "emulator-5556"), 2, 100, old)
	write(t, filepath.Join(base, "legacy", "v1-backup", "notes.txt"), 5, old)
	write(t, filepath.Join(base, "tmp", "report-123.zip"), 50, now.Add(-2*time.Hour))
	write(t, filepath.Join(base, "tmp", "fresh.tmp"), 50, now)
	write(t, filepath.Join(base, "my-notes.txt"), 5, old) // not ours
	write(t, filepath.Join(base, "photos", "a.jpg"), 5, old)

	owned := &store.Store{Dir: filepath.Join(base, "v1", "avd-owned")}
	res := Sweep(SweepOptions{Base: base, MaxAge: 24 * time.Hour, Owned: map[string]*store.Store{owned.Dir: owned}, Now: now})

	gone := []string{"v1/avd-old", "v0", "emulator-5554", "legacy", "tmp/report-123.zip",
		"v1/avd-mixed/seg-00000000000000000001.jsonl", "v1/avd-mixed/seg-00000000000000000101.jsonl"}
	for _, g := range gone {
		if exists(filepath.Join(base, g)) {
			t.Errorf("%s survived the sweep", g)
		}
	}
	kept := []string{"v1/avd-owned/seg-00000000000000000001.jsonl", "v1/avd-mixed/seg-00000000000000009999.jsonl",
		"emulator-5556/seg-00000000000000000001.jsonl", "tmp/fresh.tmp", "my-notes.txt", "photos/a.jpg", "v1"}
	for _, k := range kept {
		if !exists(filepath.Join(base, k)) {
			t.Errorf("%s was removed", k)
		}
	}
	if len(res.RemovedDirs) < 4 {
		t.Errorf("removed dirs %v", res.RemovedDirs)
	}
}

// The total cap counts every format, legacy data and tmp/ and removes the
// oldest files first; each current-format device keeps its newest segment.
func TestSweepTotalCapAcrossVersions(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	devDir(t, filepath.Join(base, "legacy", "v1-backup", "e"), 3, 1000, now.Add(-10*time.Hour)) // oldest
	devDir(t, filepath.Join(base, "emulator-5554"), 3, 1000, now.Add(-8*time.Hour))
	devDir(t, filepath.Join(base, "v1", "avd-a"), 3, 1000, now.Add(-6*time.Hour))
	write(t, filepath.Join(base, "tmp", "x.zip"), 1000, now.Add(-7*time.Hour))
	write(t, filepath.Join(base, "run", "junk.txt"), 10, now)
	total := TotalBytes(base)
	if total < 10000 {
		t.Fatalf("TotalBytes %d", total)
	}
	res := Sweep(SweepOptions{Base: base, MaxTotal: 4000, Now: now})
	if !res.CapTriggered || res.TotalAfter > 4000 {
		t.Fatalf("after sweep %d bytes (%+v)", res.TotalAfter, res)
	}
	if exists(filepath.Join(base, "legacy")) || exists(filepath.Join(base, "tmp", "x.zip")) {
		t.Error("oldest (legacy, tmp) not removed first")
	}
	if !exists(filepath.Join(base, "v1", "avd-a", "seg-00000000000000000201.jsonl")) {
		t.Error("newest segment of a current device removed")
	}
	// with an impossible cap everything but the newest current segment goes
	Sweep(SweepOptions{Base: base, MaxTotal: 1, Now: now})
	if segs := store.ListSegments(filepath.Join(base, "v1", "avd-a")); len(segs) != 1 {
		t.Errorf("avd-a has %d segments", len(segs))
	}
	if exists(filepath.Join(base, "emulator-5554")) {
		t.Error("flat legacy dir without segments not removed")
	}
}

func TestSweepSkipsLiveWriterAndOwner(t *testing.T) {
	base := t.TempDir()
	old := time.Now().Add(-72 * time.Hour)
	devDir(t, filepath.Join(base, "emulator-5554"), 2, 100, old) // an old version records it
	devDir(t, filepath.Join(base, "v1", "avd-b"), 2, 100, old)   // owned by another process
	f, ok := store.TryLock(filepath.Join(base, "emulator-5554", "writer.lock"))
	if !ok {
		t.Fatal("lock")
	}
	defer store.ReleaseLock(f)
	r := Root{Dir: base}
	l, _, err := r.AcquireOwner("avd-b", Owner{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, MaxTotal: 1})
	for _, p := range []string{"emulator-5554/seg-00000000000000000001.jsonl", "v1/avd-b/seg-00000000000000000001.jsonl"} {
		if !exists(filepath.Join(base, p)) {
			t.Errorf("%s removed although recorded by a live process", p)
		}
	}
}

func TestSweepOwnersRemovesAbandonedLocks(t *testing.T) {
	r := Root{Dir: t.TempDir()}
	l, _, err := r.AcquireOwner("live", Owner{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	// (older than a minute: without flock a fresh lock file counts as held)
	write(t, filepath.Join(r.Owners(), "crashed.lock"), 20, time.Now().Add(-2*time.Minute))
	res := Sweep(SweepOptions{Base: r.Dir, Root: &r})
	if res.StaleOwners != 1 || exists(filepath.Join(r.Owners(), "crashed.lock")) || !r.OwnerHeld("live") {
		t.Fatalf("stale owners %d; live held %v", res.StaleOwners, r.OwnerHeld("live"))
	}
}

func TestSweepRunStaleSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets in run/ are a unix feature here")
	}
	dir, err := os.MkdirTemp("", "almrun") // short path: sun_path is ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	stale := filepath.Join(dir, "stale.sock")
	ln2, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln2.(*net.UnixListener).SetUnlinkOnClose(false)
	ln2.Close() // file stays, nobody listens
	if !exists(stale) {
		t.Fatal("setup: stale socket file missing")
	}
	js := func(name string, st RunState) {
		b, _ := json.Marshal(st)
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	js("live.json", RunState{PID: os.Getpid(), Socket: live})
	js("deadpid.json", RunState{PID: 1 << 30})
	js("deadsock.json", RunState{PID: os.Getpid(), Socket: stale})
	write(t, filepath.Join(dir, "old.pid"), 3, time.Now().Add(-48*time.Hour))

	n := SweepRun(dir, 24*time.Hour, time.Now())
	for _, p := range []string{"stale.sock", "deadpid.json", "deadsock.json", "old.pid"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("%s not swept", p)
		}
	}
	for _, p := range []string{"live.sock", "live.json"} {
		if !exists(filepath.Join(dir, p)) {
			t.Errorf("%s removed although live", p)
		}
	}
	if n != 4 {
		t.Errorf("removed %d entries, want 4", n)
	}
}
