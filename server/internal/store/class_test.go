package store

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func TestLineClassExtractsFields(t *testing.T) {
	r := &model.Record{
		Seq: 7, Boot: "abcd1234", TimeNs: 1e18, UID: 1000, PID: 1, TID: 2,
		Level: model.Error, Tag: "ActivityManager", Msg: "hello \"world\"",
		Buffer: "main", Process: "system_server", Package: "android",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := lineClass(b)
	if !ok {
		t.Fatalf("lineClass failed on %s", b)
	}
	if k.pkg != "android" || k.proc != "system_server" || k.tag != "ActivityManager" || k.uid != 1000 || k.level != model.Error {
		t.Fatalf("got %+v", k)
	}
}

func TestClassCountsMatchScan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	cfg := Config{BlockBytes: 4 << 10, SegmentBytes: 64 << 10, MemRecords: 50}
	s, err := OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixNano()
	for i := 0; i < 500; i++ {
		lv := model.Info
		if i%5 == 0 {
			lv = model.Error
		}
		tag := "TagA"
		if i%3 == 0 {
			tag = "TagB"
		}
		pkg := "com.example.a"
		if i%2 == 0 {
			pkg = "com.example.b"
		}
		if err := s.Append([]*model.Record{{
			Seq: 0, Boot: "boot", TimeNs: now + int64(i)*1e6, UID: 10000 + i%3,
			PID: 100 + i%7, TID: 1, Level: lv, Tag: tag, Msg: fmt.Sprintf("m%d", i),
			Process: "proc", Package: pkg,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	s.EnsureIndex()
	matchErr := func(r *model.Record) bool { return r.Level.Rank() >= model.Error.Rank() }
	ids := s.MatchingClassIDs(matchErr)
	got := s.CountMatch(0, 0, 0, 0, ids, matchErr)
	var want uint64
	s.Scan(0, 0, true, func(r *model.Record) bool {
		if matchErr(r) {
			want++
		}
		return true
	})
	if got != want {
		t.Fatalf("CountMatch=%d Scan=%d classes=%d", got, want, s.classes.Len())
	}
	// TagB exact
	matchTag := func(r *model.Record) bool { return r.Tag == "TagB" }
	ids = s.MatchingClassIDs(matchTag)
	got = s.CountMatch(0, 0, 0, 0, ids, matchTag)
	want = 0
	s.Scan(0, 0, true, func(r *model.Record) bool {
		if matchTag(r) {
			want++
		}
		return true
	})
	if got != want {
		t.Fatalf("tag CountMatch=%d Scan=%d", got, want)
	}
}

func TestClassCapUncounted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	// tiny blocks so one block fills with many classes
	cfg := Config{BlockBytes: 1 << 20, SegmentBytes: 8 << 20, MemRecords: 10}
	s, err := OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixNano()
	n := MaxBlockClasses + 50
	for i := 0; i < n; i++ {
		if err := s.Append([]*model.Record{{
			Seq: 0, Boot: "b", TimeNs: now + int64(i), UID: i, PID: 1, TID: 1,
			Level: model.Info, Tag: fmt.Sprintf("T%d", i), Msg: "x",
			Process: fmt.Sprintf("p%d", i), Package: fmt.Sprintf("k%d", i),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	s.EnsureIndex()
	s.hmu.Lock()
	var unc bool
	for _, ix := range s.idx {
		for _, b := range ix.blocks {
			if b.uncounted {
				unc = true
			}
		}
	}
	s.hmu.Unlock()
	if !unc {
		t.Fatal("expected an uncounted block past the cap")
	}
	match := func(r *model.Record) bool { return true }
	ids := s.MatchingClassIDs(match)
	got := s.CountMatch(0, 0, 0, 0, ids, match)
	want := s.Count(0, 0)
	if got != want {
		t.Fatalf("uncounted CountMatch=%d Count=%d", got, want)
	}
}

func TestTrailingBlockKeepsCounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	cfg := Config{BlockBytes: 8 << 10, SegmentBytes: 1 << 20, MemRecords: 20}
	s, err := OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixNano()
	appendN := func(n int, tag string) {
		for i := 0; i < n; i++ {
			if err := s.Append([]*model.Record{{
				Seq: 0, Boot: "b", TimeNs: now + int64(i)*1000, UID: 1, PID: 1, TID: 1,
				Level: model.Warn, Tag: tag, Msg: fmt.Sprintf("m%d", i), Process: "p", Package: "k",
			}}); err != nil {
				t.Fatal(err)
			}
		}
		now += int64(n) * 1000
	}
	appendN(30, "A")
	s.EnsureIndex()
	matchA := func(r *model.Record) bool { return r.Tag == "A" }
	ids := s.MatchingClassIDs(matchA)
	c1 := s.CountMatch(0, 0, 0, 0, ids, matchA)
	appendN(40, "A")
	s.EnsureIndex()
	ids = s.MatchingClassIDs(matchA)
	c2 := s.CountMatch(0, 0, 0, 0, ids, matchA)
	if c1 != 30 || c2 != 70 {
		t.Fatalf("trailing: before=%d after=%d", c1, c2)
	}
}

func TestBlockSkipZeroClass(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev")
	cfg := Config{BlockBytes: 2 << 10, SegmentBytes: 64 << 10, MemRecords: 10}
	s, err := OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixNano()
	for i := 0; i < 200; i++ {
		tag := "Common"
		if i == 150 {
			tag = "Rare"
		}
		if err := s.Append([]*model.Record{{
			Seq: 0, Boot: "b", TimeNs: now + int64(i)*1e6, UID: 1, PID: 1, TID: 1,
			Level: model.Info, Tag: tag, Msg: "m", Process: "p", Package: "k",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	s.EnsureIndex()
	match := func(r *model.Record) bool { return r.Tag == "Rare" }
	ids := s.MatchingClassIDs(match)
	var visited int
	s.ScanTimeClass(0, 0, true, 0, 0, ids, func(r *model.Record) bool {
		visited++
		return true
	})
	var all int
	s.ScanTime(0, 0, true, 0, 0, func(r *model.Record) bool {
		all++
		return true
	})
	if visited >= all {
		t.Fatalf("expected class skip: visited=%d all=%d", visited, all)
	}
	if s.CountMatch(0, 0, 0, 0, ids, match) != 1 {
		t.Fatalf("rare count")
	}
}

func TestWhollyInsideUnreadable(t *testing.T) {
	b := block{minT: math.MinInt64, maxT: math.MaxInt64}
	if b.whollyInside(1, 2) {
		t.Fatal("unreadable must not be wholly inside")
	}
}
