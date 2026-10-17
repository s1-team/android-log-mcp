package layout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// #218: the sweep of a device directory nobody records uses the store's age
// rule (meta.json seg_times), not file mtimes.

func stampMeta(t *testing.T, dir string, now time.Time, ages ...time.Duration) {
	t.Helper()
	m := store.Meta{Serial: filepath.Base(dir)}
	for i, a := range ages {
		e := now.Add(-a).UnixNano()
		m.SegTimes = append(m.SegTimes, store.SegTime{First: uint64(1 + i*100), StartNs: e, EndNs: e})
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readStamps(t *testing.T, dir string) []store.SegTime {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m store.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.SegTimes
}

func segFiles(dir string) int { return len(store.ListSegments(dir)) }

func newDev(t *testing.T, n int, mtime time.Time) (base, dir string) {
	t.Helper()
	base = t.TempDir()
	dir = filepath.Join(store.DataDir(base), "dev")
	devDir(t, dir, n, 100, mtime)
	return
}

// Both stamps and mtime are far in the past (a clock that was wrong while
// recording): the store would re-anchor the stamp, so the sweep keeps the
// segment for one more MaxAge, and then lets it age out.
func TestSweepStaleStampsAndMtimeKept(t *testing.T) {
	now := time.Now()
	old := now.Add(-90 * 24 * time.Hour)
	base, dir := newDev(t, 1, old)
	stampMeta(t, dir, now, 90*24*time.Hour)
	os.Chtimes(filepath.Join(dir, "meta.json"), old, old)
	res := Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	if segFiles(dir) != 1 {
		t.Fatalf("sweep removed the segment (files=%d)", res.Files)
	}
	if st := readStamps(t, dir); len(st) != 1 || st[0].EndNs < now.Add(-time.Minute).UnixNano() {
		t.Fatalf("re-anchored stamp not persisted: %+v", st)
	}
	// a later sweep still keeps it, and one MaxAge after the correction it goes
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now.Add(30 * time.Minute)})
	if segFiles(dir) != 1 {
		t.Fatal("removed again before MaxAge after the correction")
	}
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now.Add(2 * time.Hour)})
	if segFiles(dir) != 0 {
		t.Fatal("segment never ages out after the re-anchor")
	}
}

// Stamps say recent, mtime says old (a copied/restored root, a corrected
// clock): kept.
func TestSweepRecentStampsOldMtimeKept(t *testing.T) {
	now := time.Now()
	base, dir := newDev(t, 1, now.Add(-90*24*time.Hour))
	stampMeta(t, dir, now, time.Minute)
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	if segFiles(dir) != 1 {
		t.Fatal("sweep removed a segment whose stamp is recent")
	}
}

// A directory whose stamps and mtimes are all past MaxAge is still removed.
func TestSweepGenuinelyExpiredDirRemoved(t *testing.T) {
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	base, dir := newDev(t, 2, old)
	stampMeta(t, dir, now, 3*time.Hour, 2*time.Hour)
	os.Chtimes(filepath.Join(dir, "meta.json"), old, old)
	res := Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired directory still there (segs=%d files=%d)", segFiles(dir), res.Files)
	}
}

// Expired segments go oldest first; the newest stays while it has not
// expired, whatever its mtime says.
func TestSweepKeepsNewestSegmentWhenOthersExpired(t *testing.T) {
	now := time.Now()
	base, dir := newDev(t, 3, now.Add(-90*24*time.Hour))
	stampMeta(t, dir, now, 5*time.Hour, 4*time.Hour, time.Minute)
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	segs := store.ListSegments(dir)
	if len(segs) != 1 || segs[0].First != 201 {
		t.Fatalf("segments left: %+v, want only the newest (first=201)", segs)
	}
}

// An old store without seg_times: the file mtimes decide, as before, and the
// stamps are persisted for the survivors.
func TestSweepWithoutSegTimesFallsBackToMtime(t *testing.T) {
	now := time.Now()
	base, dir := newDev(t, 3, now.Add(-5*time.Hour)) // seg i at -5h + i s: all expired
	os.Chtimes(filepath.Join(store.ListSegments(dir)[2].Path), now.Add(-time.Minute), now.Add(-time.Minute))
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	segs := store.ListSegments(dir)
	if len(segs) != 1 || segs[0].First != 201 {
		t.Fatalf("segments left: %+v, want only the fresh one", segs)
	}
	if st := readStamps(t, dir); len(st) != 1 || st[0].EndNs > now.UnixNano() || st[0].EndNs < now.Add(-2*time.Minute).UnixNano() {
		t.Fatalf("stamps not filled from the mtime: %+v", st)
	}
	// and all-expired old stores (no meta stamps) are removed entirely
	base2, dir2 := newDev(t, 2, now.Add(-5*time.Hour))
	Sweep(SweepOptions{Base: base2, MaxAge: time.Hour, Now: now})
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatal("old store without stamps, all expired, was not removed")
	}
}

// A segment dated in the future (clock was ahead) is never deleted at once,
// and ages from the sweep that saw it.
func TestSweepFutureMtimeSafe(t *testing.T) {
	now := time.Now()
	base, dir := newDev(t, 1, now.Add(48*time.Hour))
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	if segFiles(dir) != 1 {
		t.Fatal("future-dated segment deleted")
	}
	if st := readStamps(t, dir); len(st) != 1 || st[0].EndNs > now.UnixNano() {
		t.Fatalf("future stamp not clamped: %+v", st)
	}
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now.Add(2 * time.Hour)})
	if segFiles(dir) != 0 {
		t.Fatal("future-dated segment never ages out")
	}
}

// A directory held by another live writer is not touched.
func TestSweepLeavesLiveWriterDir(t *testing.T) {
	now := time.Now()
	base, dir := newDev(t, 1, now.Add(-5*time.Hour))
	stampMeta(t, dir, now, 5*time.Hour)
	f, ok := store.TryLock(filepath.Join(dir, "writer.lock"))
	if !ok {
		t.Fatal("lock")
	}
	defer store.ReleaseLock(f)
	before, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
	Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	after, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
	if segFiles(dir) != 1 || string(before) != string(after) {
		t.Fatal("a directory with a live writer.lock was changed")
	}
}

// The total cap removes the oldest segment by stamp, not by mtime.
func TestSweepCapOrdersByStamp(t *testing.T) {
	now := time.Now()
	base := t.TempDir()
	a := filepath.Join(store.DataDir(base), "a")
	b := filepath.Join(store.DataDir(base), "b")
	devDir(t, a, 2, 1000, now) // mtimes: all fresh
	devDir(t, b, 2, 1000, now.Add(-20*time.Hour))
	stampMeta(t, a, now, 10*time.Hour, time.Minute) // a's first segment is the oldest by stamp
	stampMeta(t, b, now, time.Hour, time.Minute)
	total := TotalBytes(base)
	res := Sweep(SweepOptions{Base: base, MaxTotal: total - 500, Now: now})
	if res.Files != 1 {
		t.Fatalf("removed %d files, want 1", res.Files)
	}
	if segFiles(a) != 1 || segFiles(b) != 2 {
		t.Fatalf("a has %d, b has %d segments; the cap should have removed a's oldest-stamp segment", segFiles(a), segFiles(b))
	}
}
