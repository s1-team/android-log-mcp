package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #180: a follower promoted to writer takes over what the old writer
// left: its newest seq, its segments and a torn last line.

func scanSeqs(t *testing.T, dir string) []uint64 {
	t.Helper()
	ro, err := OpenReadOnly(dir, Config{MemRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var seqs []uint64
	ro.Scan(0, 0, false, func(r *model.Record) bool { seqs = append(seqs, r.Seq); return true })
	if n := ro.Count(0, 0); n != uint64(len(seqs)) {
		t.Errorf("Count=%d but Scan returned %d records", n, len(seqs))
	}
	return seqs
}

func followerOf(t *testing.T, dir string, w *Store) *Store {
	t.Helper()
	f, err := OpenDir(dir, "x", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.IsWriter() {
		t.Fatal("expected a follower while the writer holds the lock")
	}
	t.Cleanup(f.Close)
	return f
}

// The old writer appended more after the follower last looked: the
// promoted follower continues after them, no seq is used twice.
func TestPromoteCatchesUpWithOldWriter(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	w, _ := OpenDir(dir, "x", Config{}, true)
	w.Append(mk(3, 1))
	w.Flush()
	f := followerOf(t, dir, w)
	f.Refresh()
	if f.LastSeq() != 3 {
		t.Fatalf("follower LastSeq=%d, want 3", f.LastSeq())
	}
	w.Append(mk(3, 1000)) // seqs 4..6, written after the follower's last look
	w.Close()
	if !f.TryPromote() {
		t.Fatal("promote failed")
	}
	if f.LastSeq() != 6 {
		t.Fatalf("after promote LastSeq=%d, want 6", f.LastSeq())
	}
	if err := f.Append(mk(2, 5000)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, want := scanSeqs(t, dir), []uint64{1, 2, 3, 4, 5, 6, 7, 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seqs on disk %v, want %v", got, want)
	}
}

// The old writer died in the middle of a line: the first new record does
// not join the partial line.
func TestPromoteRepairsTornTail(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	w, _ := OpenDir(dir, "x", Config{}, true)
	w.Append(mk(3, 1))
	f := followerOf(t, dir, w)
	p := filepath.Join(dir, segName(1))
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(`{"s":4,"b":"b1","t":12`) // partial line, the writer is "killed"
	fh.Close()
	w.Close()
	if !f.TryPromote() {
		t.Fatal("promote failed")
	}
	if f.LastSeq() != 3 {
		t.Fatalf("after promote LastSeq=%d, want 3", f.LastSeq())
	}
	if err := f.Append(mk(2, 100)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, want := scanSeqs(t, dir), []uint64{1, 2, 3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seqs on disk %v, want %v (no record lost)", got, want)
	}
}

// The old writer rotated to new segments the follower never listed.
func TestPromoteAfterOldWriterRotated(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	cfg := Config{SegmentBytes: 3000}
	w, _ := OpenDir(dir, "x", cfg, true)
	w.Append(mk(5, 1))
	f, _ := OpenDir(dir, "x", cfg, true)
	t.Cleanup(f.Close)
	for i := 0; i < 8; i++ {
		w.Append(mk(20, int64(i+2)*1e9))
	}
	w.Close()
	last := w.LastSeq()
	if !f.TryPromote() {
		t.Fatal("promote failed")
	}
	if f.LastSeq() != last {
		t.Fatalf("after promote LastSeq=%d, want %d", f.LastSeq(), last)
	}
	f.Append(mk(1, 99e9))
	f.Close()
	got := scanSeqs(t, dir)
	if uint64(len(got)) != last+1 || got[len(got)-1] != last+1 {
		t.Fatalf("seqs not dense and unique up to %d: len=%d tail=%v", last+1, len(got), got[len(got)-3:])
	}
	for i, s := range got {
		if s != got[0]+uint64(i) {
			t.Fatalf("seq gap or duplicate at %d: %d", i, s)
		}
	}
}

// A promotion that cannot take over (the last segment cannot be cut)
// leaves the store a follower and gives the lock back.
func TestPromoteFailureUndoesPromotion(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	w, _ := OpenDir(dir, "x", Config{}, true)
	w.Append(mk(3, 1))
	f := followerOf(t, dir, w)
	w.Close()
	// a directory where the newest segment should be: truncateTorn cannot open it
	if err := os.Mkdir(filepath.Join(dir, segName(9)), 0o755); err != nil {
		t.Fatal(err)
	}
	if f.TryPromote() {
		t.Fatal("promotion succeeded although the last segment cannot be repaired")
	}
	if f.IsWriter() {
		t.Fatal("store is a writer after a failed promotion")
	}
	lk, ok := tryLock(filepath.Join(dir, "writer.lock"))
	if !ok {
		t.Fatal("writer.lock still held after a failed promotion")
	}
	lk.Close()
	if err := f.Append(mk(1, 5)); err == nil {
		t.Fatal("a follower appended")
	}
}
