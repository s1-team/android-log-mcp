package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issues #182 / #191: a failed write rolls the batch back, leaves no
// partial line, and the writer works again.

// halfW writes half of what it is given to the file and fails, n times.
type halfW struct {
	f *os.File
	n int
}

func (w *halfW) Write(b []byte) (int, error) {
	if w.n > 0 {
		w.n--
		w.f.Write(b[:len(b)/2])
		return len(b) / 2, errors.New("disk full")
	}
	return w.f.Write(b)
}

func failNext(s *Store, n int) {
	s.mu.Lock()
	s.curW = bufio.NewWriterSize(&halfW{f: s.cur, n: n}, 4096)
	s.mu.Unlock()
}

func memSeqs(s *Store) []uint64 {
	var out []uint64
	s.Scan(0, 0, false, func(r *model.Record) bool { out = append(out, r.Seq); return true })
	return out
}

func TestAppendFailureRollsBack(t *testing.T) {
	base := t.TempDir()
	s, _ := Open(base, "x", Config{})
	if err := s.Append(mk(2, 1)); err != nil {
		t.Fatal(err)
	}
	failNext(s, 1) // the fault clears after one failed write
	batch := mk(3, 10)
	if err := s.Append(batch); err == nil {
		t.Fatal("append succeeded despite the write error")
	}
	if s.LastSeq() != 2 {
		t.Fatalf("LastSeq=%d after a failed batch, want 2 (no seq burnt)", s.LastSeq())
	}
	if got := memSeqs(s); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("window %v after a failed batch, want [1 2]", got)
	}
	// the same batch again works, with the seqs the failed call would have used
	if err := s.Append(batch); err != nil {
		t.Fatalf("append after the fault cleared: %v", err)
	}
	if err := s.Append(mk(1, 99)); err != nil {
		t.Fatal(err)
	}
	want := []uint64{1, 2, 3, 4, 5, 6}
	if got := memSeqs(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("window %v, want %v", got, want)
	}
	s.Close()
	if got := seqsOnDisk(t, filepath.Join(DataDir(base), "x")); !reflect.DeepEqual(got, want) {
		t.Fatalf("on disk after reopen %v, want %v (memory and disk agree)", got, want)
	}
}

// A flush error while rotating (rotateLocked used to ignore it) fails the
// batch and rolls it back.
func TestAppendFailureInRotation(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(DataDir(base), "x")
	s, _ := Open(base, "x", Config{SegmentBytes: 400})
	s.Append(mk(3, 1))
	s.mu.Lock()
	nsegs := len(s.segs)
	s.mu.Unlock()
	failNext(s, 1)
	s.mu.Lock()
	s.curW.WriteString("{\"s\":4,\"partial") // buffered bytes the rotation must flush
	s.curSize = s.cfg.SegmentBytes           // the next record rotates
	s.mu.Unlock()
	batch := mk(4, 10)
	if err := s.Append(batch); err == nil {
		t.Fatal("append succeeded although the rotation could not flush")
	}
	if s.LastSeq() != 3 {
		t.Fatalf("LastSeq=%d, want 3", s.LastSeq())
	}
	s.mu.Lock()
	got := len(s.segs)
	s.mu.Unlock()
	if got != nsegs {
		t.Fatalf("%d segments after the failed batch, want %d", got, nsegs)
	}
	if err := s.Append(batch); err != nil {
		t.Fatal(err)
	}
	s.Close()
	seqs := seqsOnDisk(t, dir)
	if len(seqs) != 7 {
		t.Fatalf("%d records on disk, want 7: %v", len(seqs), seqs)
	}
	for i, q := range seqs {
		if q != uint64(i+1) {
			t.Fatalf("seqs not dense and unique: %v", seqs)
		}
	}
}

func seqsOnDisk(t *testing.T, dir string) []uint64 {
	t.Helper()
	ro, err := OpenReadOnly(dir, Config{MemRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var seqs []uint64
	ro.Scan(0, 0, false, func(r *model.Record) bool { seqs = append(seqs, r.Seq); return true })
	return seqs
}

// Flush cuts bytes it could not write back to the durable size.
func TestFlushErrorCutsPartialBytes(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(DataDir(base), "x")
	s, _ := Open(base, "x", Config{})
	s.Append(mk(2, 1))
	p := filepath.Join(dir, segName(1))
	st0, _ := os.Stat(p)
	failNext(s, 1)
	s.mu.Lock()
	s.curW.WriteString("{\"s\":3,\"partial")
	s.mu.Unlock()
	s.Flush()
	if st, _ := os.Stat(p); st.Size() != st0.Size() {
		t.Fatalf("segment is %d bytes after the failed flush, want %d", st.Size(), st0.Size())
	}
	if err := s.Append(mk(1, 50)); err != nil {
		t.Fatalf("append after the failed flush: %v", err)
	}
	s.Close()
	if got, want := seqsOnDisk(t, dir), []uint64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("on disk %v, want %v", got, want)
	}
}

// failRotation makes the segments with these first seqs impossible to
// create (a directory has their name), so a rotation to them fails.
func failRotation(t *testing.T, dir string, firsts ...uint64) {
	t.Helper()
	for _, f := range firsts {
		if err := os.Mkdir(filepath.Join(dir, segName(f)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// A rotation in the middle of a failed batch: the old segment is cut back
// and its recorded size follows (a rotation had set the larger size).
func TestAppendFailureAfterRotationRestoresSegmentSize(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(DataDir(base), "x")
	s, _ := Open(base, "x", Config{SegmentBytes: 300})
	defer s.Close()
	if err := s.Append(mk(3, 1)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, segName(1))
	durable, _ := os.Stat(p)
	failRotation(t, dir, 5, 6, 7, 8, 9) // records 4-5 of the batch fit; a later one rotates and fails
	if err := s.Append(mk(6, 10)); err == nil {
		t.Fatal("append succeeded although the rotation failed")
	}
	st, _ := os.Stat(p)
	if st.Size() != durable.Size() {
		t.Fatalf("segment is %d bytes after the failed batch, want %d", st.Size(), durable.Size())
	}
	s.mu.Lock()
	recorded, last := int64(-1), s.lastSeq
	for _, sg := range s.segs {
		if sg.path == p {
			recorded = sg.size
		}
	}
	s.mu.Unlock()
	if recorded != durable.Size() {
		t.Fatalf("recorded segment size %d, want the file size %d", recorded, durable.Size())
	}
	if last != 3 {
		t.Fatalf("LastSeq=%d, want 3", last)
	}
}

// Bytes a query indexed before they were cut must not stay in the index.
func TestAppendFailureDropsStaleIndex(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(DataDir(base), "x")
	s, _ := Open(base, "x", Config{SegmentBytes: 1 << 20, MemRecords: 1})
	defer s.Close()
	s.Append(mk(3, 1))
	p := filepath.Join(dir, segName(1))
	// what a failed batch had already flushed: two lines past the durable end
	extra := mk(2, 100)
	extra[0].Seq, extra[1].Seq = 4, 5
	extra[0].Msg = "a much longer message than the ones written afterwards, so offsets differ"
	var raw []byte
	for _, r := range extra {
		b, _ := json.Marshal(r)
		raw = append(raw, append(b, '\n')...)
	}
	fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	fh.Write(raw)
	fh.Close()
	s.EnsureIndex() // a query indexes all 5 lines
	failRotation(t, dir, 4)
	s.mu.Lock()
	s.curSize = s.cfg.SegmentBytes // the batch's first record rotates, and fails
	s.mu.Unlock()
	if err := s.Append(mk(2, 200)); err == nil {
		t.Fatal("append succeeded although the rotation failed")
	}
	if err := os.Remove(filepath.Join(dir, segName(4))); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(mk(3, 300)); err != nil {
		t.Fatal(err)
	}
	s.EnsureIndex()
	s.hmu.Lock()
	ix, n := s.idx[p], 0
	for _, b := range ix.blocks {
		n += b.n
	}
	end := ix.end
	s.hmu.Unlock()
	st, _ := os.Stat(p)
	if n != 6 || end != st.Size() {
		t.Fatalf("index covers %d records up to byte %d; the segment has 6 records and %d bytes", n, end, st.Size())
	}
	all := func(*model.Record) bool { return true }
	if got := s.CountMatch(0, s.LastSeq()+1, 0, 0, s.MatchingClassIDs(all), all); got != 6 {
		t.Fatalf("CountMatch=%d, want 6", got)
	}
}
