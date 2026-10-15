package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// A recording that moves to another root (set-root) continues the seqs in
// a new segment there; moving back notes the seqs recorded elsewhere as a
// hole so line counts stay exact.
func TestContinueInOtherRoot(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	s1, err := OpenDir(a, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	s1.UpdateMeta(func(m *Meta) {
		m.Sessions = []Session{{BootID: "b1", Boot: "b1", FirstSeq: 1}}
		m.LastBootID = "b1"
	})
	appendN(t, s1, "A", 5, 1000)
	s1.AppendAux("crashes", map[string]string{"id": "c1"})
	last := s1.LastSeq()
	s1.Close()
	if err := ContinueIn(a, b, last, nil); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenDir(b, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if s2.LastSeq() != 5 || s2.Meta().LastBootID != "b1" || s2.Meta().LastTimeNs != 1004 || len(s2.Meta().Sessions) != 1 {
		t.Fatalf("state not carried: last %d meta %+v", s2.LastSeq(), s2.Meta())
	}
	if _, err := os.Stat(filepath.Join(b, "crashes.jsonl")); err != nil {
		t.Fatal("aux not carried")
	}
	appendN(t, s2, "B", 3, 2000)
	if got := seqsOf(s2, false); fmt.Sprint(got) != "[6 7 8]" {
		t.Fatalf("seqs in b: %v", got)
	}
	if _, err := os.Stat(filepath.Join(b, segName(6))); err != nil {
		t.Fatal("new root does not start a new segment at seq 6")
	}
	if st := s2.Stats(); st.Records != 3 || st.FirstSeq != 6 {
		t.Fatalf("stats %+v", st)
	}
	s2.Close()
	// back to a: its old segment (1..5) is kept, seqs continue at 9
	if err := ContinueIn(b, a, 8, nil); err != nil {
		t.Fatal(err)
	}
	s3, err := OpenDir(a, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	// queried before the first append (the recorder's -T dedup scan does
	// this): the window holds 1..5 while the last seq is the floor 8
	if s3.LastSeq() != 8 {
		t.Fatalf("last seq %d after moving back", s3.LastSeq())
	}
	var desc []uint64
	s3.Scan(0, s3.LastSeq()+1, true, func(r *model.Record) bool { desc = append(desc, r.Seq); return true })
	if fmt.Sprint(desc) != "[5 4 3 2 1]" || fmt.Sprint(seqsOf(s3, false)) != "[1 2 3 4 5]" || s3.Get(7) != nil || s3.Get(5) == nil {
		t.Fatalf("scan before append: desc %v asc %v", desc, seqsOf(s3, false))
	}
	appendN(t, s3, "A2", 2, 3000)
	if got := seqsOf(s3, false); fmt.Sprint(got) != "[1 2 3 4 5 9 10]" {
		t.Fatalf("seqs in a: %v", got)
	}
	if _, err := os.Stat(filepath.Join(a, segName(9))); err != nil {
		t.Fatal("no new segment after the seq jump")
	}
	if st := s3.Stats(); st.Records != 7 {
		t.Fatalf("records %d, want 7 (hole 6..8 excluded)", st.Records)
	}
	// reopen: floor and holes persist
	s3.Close()
	s4, _ := OpenReadOnly(a, Config{})
	defer s4.Close()
	if s4.LastSeq() != 10 || s4.Stats().Records != 7 {
		t.Fatalf("reopen: last %d records %d", s4.LastSeq(), s4.Stats().Records)
	}
}

func TestContinueInSameDirRefused(t *testing.T) {
	d := t.TempDir()
	if ContinueIn(d, d, 1, nil) == nil {
		t.Fatal("same dir accepted")
	}
}

func TestResumeDedupLifetime(t *testing.T) {
	rd := &ResumeDedup{BootID: "b1", SeqFloor: 1000, Count: 50}
	for _, c := range []struct {
		boot string
		last uint64
		want bool
	}{{"b1", 1000, true}, {"b1", 1049, true}, {"b1", 1050, false}, {"b2", 1000, false}, {"b1", 10, true}} {
		if got := rd.Active(c.boot, 0, c.last); got != c.want {
			t.Errorf("Active(%s, %d) = %v, want %v", c.boot, c.last, got, c.want)
		}
	}
	if (*ResumeDedup)(nil).Active("b1", 0, 0) {
		t.Error("nil is never active")
	}
}

func TestContinueInCarriedFile(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	s1, err := OpenDir(a, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	s1.UpdateMeta(func(m *Meta) { m.LastBootID = "b1" })
	appendN(t, s1, "A", 3, 1000)
	s1.Close()
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(b, ResumeDedupFile), []byte("stale"), 0o644)
	if err := ContinueIn(a, b, 3, []uint64{7, 7, 9}); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenDir(b, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rd := s2.Meta().ResumeDedup; rd == nil || rd.Count != 3 || rd.BootID != "b1" || rd.SeqFloor != 3 || rd.Hashes != nil {
		t.Fatalf("meta: %+v", rd)
	}
	if h := s2.LoadResumeDedup(); fmt.Sprint(h) != "[7 7 9]" {
		t.Fatalf("hashes %v", h)
	}
	// a damaged file is ignored (the stored lines remain the only de-dup)
	os.WriteFile(filepath.Join(b, ResumeDedupFile), []byte("short"), 0o644)
	if h := s2.LoadResumeDedup(); h != nil {
		t.Fatalf("damaged file read: %v", h)
	}
	s2.ClearResumeDedup()
	if _, err := os.Stat(filepath.Join(b, ResumeDedupFile)); !os.IsNotExist(err) || s2.Meta().ResumeDedup != nil {
		t.Fatal("not cleared")
	}
	// nothing to carry (or no boot): no file, and a stale one is removed
	os.WriteFile(filepath.Join(b, ResumeDedupFile), []byte("stale"), 0o644)
	s2.Close()
	if err := ContinueIn(a, b, 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, ResumeDedupFile)); !os.IsNotExist(err) {
		t.Fatal("stale file kept")
	}
}
