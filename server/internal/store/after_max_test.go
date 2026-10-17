package store

import (
	"math"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #188: after == MaxUint64 must select nothing; after+1 used to wrap
// to 0 and return everything.
func TestAfterMaxUint64SelectsNothing(t *testing.T) {
	s, _ := Open(t.TempDir(), "d", Config{SegmentBytes: 1000, MemRecords: 5, BlockBytes: 300})
	defer s.Close()
	s.Append(mk(200, 1))
	s.Flush()
	s.EnsureIndex()
	all := func(*model.Record) bool { return true }
	for _, before := range []uint64{0, 1, 50, math.MaxUint64} {
		var n int
		s.Scan(math.MaxUint64, before, false, func(*model.Record) bool { n++; return true })
		s.ScanTimeClass(math.MaxUint64, before, true, 0, 0, nil, func(*model.Record) bool { n++; return true })
		if n != 0 {
			t.Errorf("before=%d: Scan visited %d records, want 0", before, n)
		}
		if c := s.Count(math.MaxUint64, before); c != 0 {
			t.Errorf("before=%d: Count=%d, want 0", before, c)
		}
		if c := s.CountMatch(math.MaxUint64, before, 0, 0, nil, all); c != 0 {
			t.Errorf("before=%d: CountMatch=%d, want 0", before, c)
		}
		if total, _, _, _ := s.AggClasses(math.MaxUint64, before, 0, 0, nil, all); total != 0 {
			t.Errorf("before=%d: AggClasses total=%d, want 0", before, total)
		}
	}
	if s.Get(0) != nil {
		t.Error("Get(0) returned a record")
	}
}

// Issue #188: nil class ids mean "every class" everywhere, not "no class"
// on the block-count path.
func TestNilClassIDsMeanAllClasses(t *testing.T) {
	s, _ := Open(t.TempDir(), "d", Config{SegmentBytes: 1000, MemRecords: 5, BlockBytes: 300})
	defer s.Close()
	s.Append(mk(200, 1))
	s.Flush()
	s.EnsureIndex()
	all := func(*model.Record) bool { return true }
	var scanned uint64
	s.ScanTimeClass(0, 0, false, 0, 0, nil, func(*model.Record) bool { scanned++; return true })
	if scanned != 200 {
		t.Fatalf("ScanTimeClass(nil) visited %d, want 200", scanned)
	}
	if got := s.CountMatch(0, 0, 0, 0, nil, all); got != scanned {
		t.Errorf("CountMatch(nil ids)=%d, want %d", got, scanned)
	}
	if total, _, _, _ := s.AggClasses(0, 0, 0, 0, nil, all); uint64(total) != scanned {
		t.Errorf("AggClasses(nil ids) total=%d, want %d", total, scanned)
	}
	// an empty (non-nil) set still means "no class matches"
	none := map[uint32]bool{}
	if got := s.CountMatch(0, 0, 0, 0, none, func(*model.Record) bool { return false }); got != 0 {
		t.Errorf("CountMatch(empty ids)=%d, want 0", got)
	}
}
