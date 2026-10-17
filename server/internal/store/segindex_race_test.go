package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #197: segIndexFor handed out the shared index while extendIndex
// reopens the trailing block and appends in place. A writer that keeps
// extending the index while searches read it is a data race (run with -race).
func TestSegIndexExtendWhileSearching(t *testing.T) {
	cfg := Config{BlockBytes: 2 << 10, SegmentBytes: 64 << 10, MemRecords: 5}
	s, err := OpenDir(filepath.Join(t.TempDir(), "dev"), "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec := func(i int) *model.Record {
		return &model.Record{Boot: "b", TimeNs: int64(1000 + i), UID: 1, PID: 1, TID: 1, Level: model.Info,
			Tag: fmt.Sprintf("T%d", i%20), Msg: "m", Process: "p", Package: "k"}
	}
	if err := s.Append([]*model.Record{rec(0)}); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop.Store(true)
		end := time.Now().Add(1500 * time.Millisecond)
		for i := 1; time.Now().Before(end); i++ {
			s.Append([]*model.Record{rec(i)})
		}
	}()
	all := func(*model.Record) bool { return true }
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				upper := s.LastSeq() + 1
				s.EnsureIndex()
				ids := s.MatchingClassIDs(all)
				if got := s.CountMatch(0, upper, 0, 0, ids, all); got > upper-1 {
					t.Errorf("CountMatch(0,%d)=%d, more than stored", upper, got)
				}
				s.Scan(0, upper, false, func(*model.Record) bool { return true })
			}
		}()
	}
	wg.Wait()
}
