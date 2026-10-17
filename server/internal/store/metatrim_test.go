package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #173 G6: meta.json bookkeeping (sessions, clock jumps, segment stamps) is
// trimmed together with pruned segments.

func trimStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	cfg := Config{SegmentBytes: 3000, MaxAge: time.Hour, IdleRelease: -1}
	setHost(t, time.Now())
	s, err := Open(base, "x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for i := 0; i < 10; i++ {
		s.Append(mk(20, int64(i)*1e9))
	}
	if n := nSegs(s.Dir); n < 5 {
		t.Fatalf("setup: %d segments", n)
	}
	return s, base
}

// removeUntil drops oldest segments while the first retained seq is below want.
func removeUntil(t *testing.T, s *Store, want uint64) {
	t.Helper()
	for s.FirstSeq() < want {
		segs, _ := s.listSegments()
		if len(segs) < 2 || segs[1].first > want {
			break
		}
		if _, ok := s.RemoveOldestSegment(segs[0].path); !ok {
			t.Fatal("RemoveOldestSegment failed")
		}
	}
}

func TestPruneTrimsSessionsKeepingTheCoveringOne(t *testing.T) {
	s, _ := trimStore(t)
	segs, _ := s.listSegments()
	cut := segs[3].first // first retained seq after removing three segments
	s.UpdateMeta(func(m *Meta) {
		m.Sessions = []Session{
			{BootID: "a-1", Boot: "a", FirstSeq: 1},
			{BootID: "b-1", Boot: "b", FirstSeq: 5},   // covers the first retained seq
			{BootID: "c-1", Boot: "c", FirstSeq: cut}, // starts exactly there
			{BootID: "d-1", Boot: "d", FirstSeq: cut + 30},
		}
	})
	removeUntil(t, s, cut)
	if s.FirstSeq() != cut {
		t.Fatalf("first seq %d, want %d", s.FirstSeq(), cut)
	}
	got := s.Meta().Sessions
	var boots []string
	for _, se := range got {
		boots = append(boots, se.Boot)
	}
	// "a" and "b" ended before the range; "c" starts at its first seq and
	// covers it, so it is the first one kept.
	if strings.Join(boots, "") != "cd" {
		t.Fatalf("sessions after trim: %v, want [c d]", boots)
	}
}

func TestPruneKeepsSessionSpanningTheFirstRetainedSeq(t *testing.T) {
	s, _ := trimStore(t)
	segs, _ := s.listSegments()
	cut := segs[3].first
	s.UpdateMeta(func(m *Meta) {
		m.Sessions = []Session{
			{BootID: "a-1", Boot: "a", FirstSeq: 1},
			{BootID: "b-1", Boot: "b", FirstSeq: 5}, // began in a pruned segment, still running
		}
	})
	removeUntil(t, s, cut)
	got := s.Meta().Sessions
	if len(got) != 1 || got[0].Boot != "b" {
		t.Fatalf("sessions %+v, want only b (it covers the first retained seq)", got)
	}
	// the boot of a retained seq is still found the way readers do it
	boot := "0"
	for _, se := range got {
		if se.FirstSeq <= s.FirstSeq()+1 {
			boot = se.Boot
		}
	}
	if boot != "b" {
		t.Fatalf("boot of first retained seq = %q", boot)
	}
}

func TestPruneKeepsTheOnlySession(t *testing.T) {
	s, _ := trimStore(t)
	s.UpdateMeta(func(m *Meta) { m.Sessions = []Session{{BootID: "a-1", Boot: "a", FirstSeq: 1}} })
	segs, _ := s.listSegments()
	removeUntil(t, s, segs[3].first)
	if got := s.Meta().Sessions; len(got) != 1 || got[0].Boot != "a" {
		t.Fatalf("sessions %+v, want a kept", got)
	}
}

func TestPruneTrimsClockJumps(t *testing.T) {
	s, _ := trimStore(t)
	segs, _ := s.listSegments()
	cut := segs[3].first
	for _, seq := range []uint64{3, cut - 1, cut, cut + 10, s.LastSeq() - 1} { // before, before, at, inside, after
		s.AddClockJump(ClockJump{Seq: seq, DeltaNs: 3600e9, Source: "set"})
	}
	if n := len(s.Meta().ClockJumps); n != 5 {
		t.Fatalf("setup: %d jumps", n)
	}
	removeUntil(t, s, cut)
	var seqs []uint64
	for _, j := range s.Meta().ClockJumps {
		seqs = append(seqs, j.Seq)
	}
	want := []uint64{cut, cut + 10, s.LastSeq() - 1}
	if len(seqs) != 3 || seqs[0] != want[0] || seqs[1] != want[1] || seqs[2] != want[2] {
		t.Fatalf("stored jumps %v, want %v", seqs, want)
	}
	// the read view is unchanged by the trim
	if got := s.ClockJumps(); len(got) != 3 {
		t.Fatalf("ClockJumps() = %d, want 3", len(got))
	}
}

// Prune by age trims the same way and keeps SegTimes consistent with the
// retained segments (#210).
func TestPruneByAgeTrimsMetaWithSegTimes(t *testing.T) {
	s, base := trimStore(t)
	last := s.segs[len(s.segs)-1].first
	s.UpdateMeta(func(m *Meta) {
		m.Sessions = []Session{{BootID: "a-1", Boot: "a", FirstSeq: 1}, {BootID: "b-1", Boot: "b", FirstSeq: last}}
	})
	s.AddClockJump(ClockJump{Seq: 7, DeltaNs: 3600e9, Source: "set"})
	setHost(t, time.Now().Add(2*time.Hour)) // everything but the active segment is past MaxAge
	s.Prune()
	if nSegs(s.Dir) != 1 {
		t.Fatalf("%d segments, want 1", nSegs(s.Dir))
	}
	m := s.Meta()
	if len(m.Sessions) != 1 || m.Sessions[0].Boot != "b" {
		t.Fatalf("sessions %+v", m.Sessions)
	}
	if len(m.ClockJumps) != 0 {
		t.Fatalf("jumps %+v", m.ClockJumps)
	}
	if len(m.SegTimes) != 1 || m.SegTimes[0].First != last {
		t.Fatalf("seg_times %+v, want one stamp for the active segment %d", m.SegTimes, last)
	}
	// persisted compactly, readable, and an old reader's view (plain struct
	// without seg_times) is the same data
	s.Flush()
	b, err := os.ReadFile(filepath.Join(s.Dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n") {
		t.Errorf("meta.json is indented: %q", b)
	}
	var old struct {
		Sessions   []Session
		ClockJumps []ClockJump `json:"clock_jumps"`
	}
	if err := json.Unmarshal(b, &old); err != nil || len(old.Sessions) != 1 {
		t.Fatalf("old reader: %v %+v", err, old)
	}
	s.Close()
	r, err := Open(base, "x", Config{MaxAge: time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Meta().Sessions; len(got) != 1 || got[0].Boot != "b" {
		t.Fatalf("reopened sessions %+v", got)
	}
}
