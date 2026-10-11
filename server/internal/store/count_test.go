package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// scanCount is the reference: what Scan visits.
func scanCount(s *Store, after, before uint64) uint64 {
	var n uint64
	s.Scan(after, before, true, func(*model.Record) bool { n++; return true })
	return n
}

func checkCounts(t *testing.T, s *Store) {
	t.Helper()
	last := s.LastSeq()
	for after := uint64(0); after <= last+1; after += max(1, last/37) {
		for _, before := range []uint64{0, after + 1, after + 2, after + 50, after + 777, last, last + 1, last + 5} {
			if got, want := s.Count(after, before), scanCount(s, after, before); got != want {
				t.Fatalf("Count(%d, %d) = %d, Scan visits %d (last %d)", after, before, got, want, last)
			}
		}
	}
}

// Issue #20: Count equals what Scan visits: across segments and blocks
// (small blocks and segments), with the in-memory window, after a seq jump
// (an empty newest segment) and for a read-only follower.
func TestCountMatchesScan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	cfg := Config{BlockBytes: 4 << 10, SegmentBytes: 64 << 10, MemRecords: 300}
	s, err := OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	appendN(t, s, "A", 5000, 1000)
	checkCounts(t, s)
	ro, err := OpenReadOnly(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	checkCounts(t, ro)
	ro.ReleaseCaches()
	checkCounts(t, ro)
}

// A seq jump (empty newest segment): LastSeq is 8 but only 1..5 exist.
func TestCountAfterSeqJump(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	s1, err := OpenDir(dir, "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, s1, "A", 5, 1000)
	s1.Close()
	if err := os.WriteFile(filepath.Join(dir, segName(9)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenReadOnly(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.LastSeq() != 8 || s.Count(0, 0) != 5 {
		t.Fatalf("last %d, count %d", s.LastSeq(), s.Count(0, 0))
	}
	checkCounts(t, s)
}
