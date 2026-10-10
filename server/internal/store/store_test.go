package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func mk(n int, t0 int64) []*model.Record {
	out := make([]*model.Record, n)
	for i := range out {
		out[i] = &model.Record{TimeNs: t0 + int64(i)*1e6, PID: 100 + i%3, TID: 1, Level: 'I', Tag: "T", Msg: fmt.Sprintf("msg %d", i), Boot: "b1", UID: -1}
	}
	return out
}

func collect(s *Store, after, before uint64, desc bool, limit int) []uint64 {
	var out []uint64
	s.Scan(after, before, desc, func(r *model.Record) bool {
		out = append(out, r.Seq)
		return len(out) < limit
	})
	return out
}

func TestAppendScanReopen(t *testing.T) {
	base := tempDir(t)
	cfg := Config{SegmentBytes: 2000, MemRecords: 50}
	s, err := Open(base, "emulator-5554", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsWriter() {
		t.Fatal("expected writer")
	}
	if err := s.Append(mk(300, 1e18)); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	if s.LastSeq() != 300 {
		t.Fatalf("last %d", s.LastSeq())
	}
	segs, _ := s.listSegments()
	if len(segs) < 5 {
		t.Fatalf("expected rotation, got %d segments", len(segs))
	}
	// ascending across disk+mem boundary
	got := collect(s, 10, 0, false, 1000)
	if len(got) != 290 || got[0] != 11 || got[len(got)-1] != 300 {
		t.Fatalf("asc: n=%d first=%v last=%v", len(got), got[0], got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("asc not contiguous at %d", i)
		}
	}
	// descending, bounded
	got = collect(s, 0, 280, true, 5)
	if fmt.Sprint(got) != "[279 278 277 276 275]" {
		t.Fatalf("desc %v", got)
	}
	got = collect(s, 0, 0, true, 1000)
	if len(got) != 300 || got[0] != 300 || got[299] != 1 {
		t.Fatalf("desc all %d", len(got))
	}
	s.Close()

	// reopen: seq continues, mem filled from tail
	s2, err := Open(base, "emulator-5554", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s2.LastSeq() != 300 {
		t.Fatalf("reopen last %d", s2.LastSeq())
	}
	s2.Append(mk(2, 2e18))
	if s2.LastSeq() != 302 {
		t.Fatal("seq continue")
	}
	if r := s2.Get(150); r == nil || r.Msg != "msg 149" {
		t.Fatalf("get %v", r)
	}
	s2.Close()
}

func TestTornLineRepair(t *testing.T) {
	base := tempDir(t)
	s, _ := Open(base, "x", Config{})
	s.Append(mk(3, 1))
	s.Close()
	p := filepath.Join(base, "x", segName(1))
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"s":4,"b":"b1","t":`)
	f.Close()
	s2, _ := Open(base, "x", Config{})
	if s2.LastSeq() != 3 {
		t.Fatalf("last %d", s2.LastSeq())
	}
	s2.Append(mk(1, 5))
	s2.Close()
	s3, _ := Open(base, "x", Config{})
	if s3.LastSeq() != 4 || s3.Get(4) == nil {
		t.Fatal("after repair")
	}
	s3.Close()
}

func TestFollowerTailsWriter(t *testing.T) {
	base := tempDir(t)
	w, _ := Open(base, "dev", Config{})
	f, _ := Open(base, "dev", Config{})
	if !w.IsWriter() || f.IsWriter() {
		t.Fatalf("writer=%v follower=%v", w.IsWriter(), f.IsWriter())
	}
	w.Append(mk(10, 1))
	w.Flush()
	f.Refresh()
	if f.LastSeq() != 10 {
		t.Fatalf("follower last %d", f.LastSeq())
	}
	done := make(chan bool)
	go func() { done <- f.Wait(context.Background(), 10, 3*time.Second) }()
	time.Sleep(100 * time.Millisecond)
	w.Append(mk(1, 100))
	w.Flush()
	if !<-done {
		t.Fatal("follower wait did not see new record")
	}
	// writer exits -> follower can promote and continue the sequence
	w.Close()
	if !f.TryPromote() {
		t.Fatal("promote")
	}
	f.Append(mk(1, 200))
	if f.LastSeq() != 12 {
		t.Fatalf("after promote last %d", f.LastSeq())
	}
	f.Close()
}

func TestWaitTimeoutAndNotify(t *testing.T) {
	s, _ := Open(tempDir(t), "d", Config{})
	t.Cleanup(s.Close)
	start := time.Now()
	if s.Wait(context.Background(), 0, 200*time.Millisecond) {
		t.Fatal("should time out")
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("returned too early")
	}
	go func() { time.Sleep(50 * time.Millisecond); s.Append(mk(1, 1)) }()
	if !s.Wait(context.Background(), 0, 2*time.Second) {
		t.Fatal("should be notified")
	}
}

func TestRetentionBySize(t *testing.T) {
	base := tempDir(t)
	s, _ := Open(base, "d", Config{SegmentBytes: 1000, MaxBytes: 5000})
	t.Cleanup(s.Close)
	s.Append(mk(500, 1))
	s.Flush()
	var total int64
	segs, _ := s.listSegments()
	for _, sg := range segs {
		total += sg.size
	}
	if total > 6000 {
		t.Fatalf("retention not applied: %d bytes in %d segs", total, len(segs))
	}
	if s.FirstSeq() <= 1 {
		t.Fatal("oldest should be pruned")
	}
	// scanning below the oldest available seq just yields what exists
	got := collect(s, 0, 0, false, 1)
	if len(got) != 1 || got[0] != s.FirstSeq() {
		t.Fatalf("first available %v vs %d", got, s.FirstSeq())
	}
}

func TestCursor(t *testing.T) {
	c := Cursor{Serial: "emulator-5554", Boot: "9f2a1b3c", Seq: 1204331}
	p, err := ParseCursor(c.String())
	if err != nil || p != c {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := ParseCursor("garbage"); err == nil {
		t.Fatal("expected error")
	}
}

func TestAux(t *testing.T) {
	s, _ := Open(tempDir(t), "d", Config{})
	t.Cleanup(s.Close)
	s.AppendAux("crashes", map[string]int{"a": 1})
	s.AppendAux("crashes", map[string]int{"a": 2})
	if n := len(s.ReadAux("crashes")); n != 2 {
		t.Fatal(n)
	}
}

// Regression for the one-time "reader 3.5 min behind" observation: a
// follower must see every appended batch immediately, without waiting for
// the writer's periodic Flush, including across a large backlog import and
// a fresh follower process opening mid-stream.
func TestFollowerSeesAppendsWithoutFlush(t *testing.T) {
	base := tempDir(t)
	w, _ := Open(base, "dev", Config{SegmentBytes: 64 << 10})
	defer w.Close()
	for i := 0; i < 50; i++ { // backlog import in batches, spanning several segments
		w.Append(mk(200, int64(i)*1e9))
	}
	f, _ := Open(base, "dev", Config{SegmentBytes: 64 << 10})
	if f.IsWriter() {
		t.Fatal("follower became writer")
	}
	f.Refresh()
	if f.LastSeq() != w.LastSeq() {
		t.Fatalf("fresh follower at %d, writer at %d", f.LastSeq(), w.LastSeq())
	}
	for i := 0; i < 5; i++ {
		w.Append(mk(3, int64(100+i)*1e9)) // no Flush
		f.Refresh()
		if f.LastSeq() != w.LastSeq() {
			t.Fatalf("batch %d: follower at %d, writer at %d", i, f.LastSeq(), w.LastSeq())
		}
		if st := f.Stats(); st.NewestNs != int64(100+i)*1e9+2e6 {
			t.Fatalf("newest %d", st.NewestNs)
		}
	}
}
