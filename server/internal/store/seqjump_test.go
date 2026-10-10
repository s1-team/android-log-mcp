package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func appendN(t *testing.T, s *Store, tag string, n int, t0 int64) {
	t.Helper()
	var rs []*model.Record
	for i := 0; i < n; i++ {
		rs = append(rs, &model.Record{TimeNs: t0 + int64(i), Level: 'I', Tag: tag, Msg: fmt.Sprint(i), Boot: "b1"})
	}
	if err := s.Append(rs); err != nil {
		t.Fatal(err)
	}
	s.Flush()
}

func seqsOf(s *Store, desc bool) []uint64 {
	var out []uint64
	s.Scan(0, s.LastSeq()+1, desc, func(r *model.Record) bool { out = append(out, r.Seq); return true })
	return out
}

// The last seq can lie above the in-memory window: an empty newest segment
// after a seq jump (seg-9 with nothing in it after 1..5) makes LastSeq 8
// while the window ends at 5. Scan must stay inside the window (this
// crashed the live recorder in #9: index out of range).
func TestScanWithLastSeqAboveWindow(t *testing.T) {
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
	for _, writer := range []bool{false, true} {
		var s *Store
		if writer {
			s, err = OpenDir(dir, "dev", Config{}, true)
		} else {
			s, err = OpenReadOnly(dir, Config{})
		}
		if err != nil {
			t.Fatal(err)
		}
		if writer {
			// the writer's open-time prune drops the empty segment: the
			// seqs then simply continue at 6
			if s.LastSeq() != 5 {
				t.Fatalf("writer: last seq %d, want 5", s.LastSeq())
			}
			appendN(t, s, "B", 2, 2000)
			if got := fmt.Sprint(seqsOf(s, true)); got != "[7 6 5 4 3 2 1]" {
				t.Fatalf("writer desc %s", got)
			}
			s.Close()
			continue
		}
		if s.LastSeq() != 8 {
			t.Fatalf("last seq %d, want 8", s.LastSeq())
		}
		if got := fmt.Sprint(seqsOf(s, true)); got != "[5 4 3 2 1]" {
			t.Fatalf("writer=%v: desc %s", writer, got)
		}
		if got := fmt.Sprint(seqsOf(s, false)); got != "[1 2 3 4 5]" {
			t.Fatalf("writer=%v: asc %s", writer, got)
		}
		if s.Get(7) != nil || s.Get(5) == nil {
			t.Fatalf("writer=%v: Get", writer)
		}
		s.Close()
	}
}
