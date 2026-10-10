package device

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func seqList(st *store.Store) []uint64 {
	var out []uint64
	st.Scan(0, st.LastSeq()+1, false, func(r *model.Record) bool { out = append(out, r.Seq); return true })
	return out
}

// runSetRoot runs set-root while the managers tick (SyncRoot) like their
// maintenance loops do.
func runSetRoot(t *testing.T, opt layout.SetRootOptions, ms ...*Manager) layout.SetRootResult {
	t.Helper()
	type out struct {
		r   layout.SetRootResult
		err error
	}
	ch := make(chan out, 1)
	opt.Poll, opt.Timeout = 10*time.Millisecond, 10*time.Second
	go func() { r, err := layout.SetRoot(opt); ch <- out{r, err} }()
	for {
		select {
		case o := <-ch:
			if o.err != nil {
				t.Fatalf("set-root: %v (%+v)", o.err, o.r)
			}
			return o.r
		case <-time.After(20 * time.Millisecond):
			for _, m := range ms {
				m.SyncRoot()
			}
		}
	}
}

// The live switch (issue #9): a recording process moves the device to the
// new root on its maintenance tick, seqs continue without a gap in a new
// segment there, the owner lock acknowledges, set-root then handles the old
// root; a follower re-follows the new dir; switching back with --delete-old
// removes the other root and the seqs still never repeat.
func TestLiveRootSwitch(t *testing.T) {
	os.Remove(layout.ConfigPath())
	t.Cleanup(func() { os.Remove(layout.ConfigPath()) })
	owners := t.TempDir()
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	if err := layout.WriteConfig(layout.Config{Root: rootA}); err != nil {
		t.Fatal(err)
	}
	id := Identity{ID: "avd-a", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"}
	m := testManager(t, rootA, owners)
	m.FollowConfig = true
	m.Attach(id)
	d := m.Get("avd-a")
	if d == nil || !d.Owned() {
		t.Fatal("not owned")
	}
	t0 := time.Now().UnixNano()
	d.Store.UpdateMeta(func(mt *store.Meta) {
		mt.Sessions = []store.Session{{BootID: "b1", Boot: "b1", FirstSeq: 1}}
		mt.LastBootID = "b1"
	})
	d.Store.Append(recs(5, t0, "b1"))
	d.Store.Flush()
	gapsBefore := len(d.Store.Meta().Gaps)

	// a reader (another MCP process, same root) follows the owner
	f := testManager(t, rootA, owners)
	f.FollowConfig = true
	f.Attach(id)
	if fd := f.Get("avd-a"); fd == nil || fd.Owned() || fd.Store.Dir != d.Store.Dir {
		t.Fatal("reader does not follow")
	}

	res := runSetRoot(t, layout.SetRootOptions{Target: rootB, KeepOld: true, OwnersDir: owners}, m, f)
	if res.Outcome != "switched" || len(res.Moved) != 1 {
		t.Fatalf("%+v", res)
	}
	nd := m.Get("avd-a")
	dirB := layout.DeviceDir(res.NewRoot, "avd-a")
	if nd == nil || !nd.Owned() || !nd.Store.IsWriter() || nd.Store.Dir != dirB {
		t.Fatalf("not moved: %+v", nd)
	}
	if m.Base != res.NewRoot {
		t.Fatalf("base %s", m.Base)
	}
	o, held := m.Root.ReadOwner("avd-a")
	if !held || o.Root != res.NewRoot || o.DataDir != dirB {
		t.Fatalf("owner lock (ack) %+v held %v", o, held)
	}
	if mt := nd.Store.Meta(); mt.LastBootID != "b1" || mt.LastTimeNs != t0+4e6 || len(mt.Gaps) != gapsBefore {
		t.Fatalf("resume state not carried / gap added: %+v", mt)
	}
	if len(nd.dedupSeed) == 0 {
		t.Fatal("no dedup seed for the logcat -T boundary")
	}
	nd.Store.Append(recs(3, t0+1e9, "b1"))
	nd.Store.Flush()
	if got := fmt.Sprint(seqList(nd.Store)); got != "[6 7 8]" {
		t.Fatalf("seqs in new root %s", got)
	}
	if _, err := os.Stat(filepath.Join(dirB, fmt.Sprintf("seg-%020d.jsonl", 6))); err != nil {
		t.Fatal("no new segment in the new root")
	}
	// old root kept (not deleted), never written again
	oldDir := layout.DeviceDir(res.OldRoot, "avd-a")
	old, err := store.OpenReadOnly(oldDir, store.Config{})
	if err != nil || old.LastSeq() != 5 {
		t.Fatalf("old root: %v last %d", err, old.LastSeq())
	}
	old.Close()
	// the reader moved too and follows the new dir
	f.Attach(id)
	fd := f.Get("avd-a")
	if f.Base != res.NewRoot || fd == nil || fd.Owned() || fd.Store.Dir != dirB {
		t.Fatalf("reader: base %s dev %+v", f.Base, fd)
	}
	fd.Store.Refresh()
	if fd.Store.LastSeq() != 8 {
		t.Fatalf("reader sees last seq %d", fd.Store.LastSeq())
	}

	// back to A, deleting B: A's old segment stays, seqs continue at 9
	res2 := runSetRoot(t, layout.SetRootOptions{Target: rootA, DeleteOld: true, OwnersDir: owners}, m, f)
	if res2.Outcome != "switched" || !res2.OldRootRemoved {
		t.Fatalf("%+v", res2)
	}
	if _, err := os.Stat(rootB); err == nil {
		t.Fatal("old root B not removed")
	}
	back := m.Get("avd-a")
	if back == nil || back.Store.Dir != oldDir || !back.Store.IsWriter() {
		t.Fatalf("not back in A: %+v", back)
	}
	// the recorder's logcat -T dedup scan runs before the first append: the
	// window (1..5) ends below the floor (8); this crashed the live recorder
	if h := boundaryHashes(back.Store); back.Store.LastSeq() != 8 || len(h) != 0 {
		t.Fatalf("back in A: last %d boundary %d", back.Store.LastSeq(), len(h))
	}
	back.Store.Append(recs(2, t0+2e9, "b1"))
	back.Store.Flush()
	if got := fmt.Sprint(seqList(back.Store)); got != "[1 2 3 4 5 9 10]" {
		t.Fatalf("seqs back in A %s", got)
	}
	if st := back.Store.Stats(); st.Records != 7 {
		t.Fatalf("records %d", st.Records)
	}
}

// With no recorder running, set-root switches at once and a process started
// afterwards records into the new root.
func TestSetRootNoRecorderImmediate(t *testing.T) {
	os.Remove(layout.ConfigPath())
	t.Cleanup(func() { os.Remove(layout.ConfigPath()) })
	owners := t.TempDir()
	rootB := filepath.Join(t.TempDir(), "b")
	start := time.Now()
	res, err := layout.SetRoot(layout.SetRootOptions{Target: rootB, KeepOld: true, OwnersDir: owners, Timeout: time.Minute})
	if err != nil || res.Outcome != "switched" || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v %v after %s", res, err, time.Since(start))
	}
	base, _ := layout.ConfiguredRoot()
	m := testManager(t, base, owners)
	m.FollowConfig = true
	m.Attach(Identity{ID: "avd-n", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"})
	if d := m.Get("avd-n"); d == nil || d.Store.Dir != layout.DeviceDir(res.NewRoot, "avd-n") {
		t.Fatal("new process does not record into the new root")
	}
}

// A process whose root is stale never takes ownership into it: attach
// re-reads the config first.
func TestAttachUsesCurrentConfig(t *testing.T) {
	os.Remove(layout.ConfigPath())
	t.Cleanup(func() { os.Remove(layout.ConfigPath()) })
	owners := t.TempDir()
	rootA, rootB := t.TempDir(), t.TempDir()
	layout.WriteConfig(layout.Config{Root: rootA})
	m := testManager(t, rootA, owners)
	m.FollowConfig = true
	layout.WriteConfig(layout.Config{Root: rootB}) // set-root by another process
	m.Attach(Identity{ID: "avd-s", Source: "avd_name", Addr: "emulator-5554", BootID: "b1"})
	if d := m.Get("avd-s"); d == nil || d.Store.Dir != layout.DeviceDir(rootB, "avd-s") {
		t.Fatal("recorded into the stale root")
	}
	if _, err := os.Stat(layout.DeviceDir(rootA, "avd-s")); err == nil {
		t.Fatal("stale root got a device dir")
	}
}
