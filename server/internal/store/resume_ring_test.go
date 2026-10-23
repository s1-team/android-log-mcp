package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func ringRec(seq uint64, ns int64, msg string) *model.Record {
	return &model.Record{
		Seq: seq, TimeNs: ns, PID: 100, TID: 100, Level: 'I', Tag: "T",
		Msg: msg, Boot: "b", UID: -1,
	}
}

func openRingStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenDir(filepath.Join(t.TempDir(), "dev"), "dev", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedBoot(st *Store, boot string, firstSeq uint64) {
	st.UpdateMeta(func(m *Meta) {
		m.LastBootID = boot
		m.Sessions = []Session{{BootID: boot, Boot: boot, FirstSeq: firstSeq}}
	})
}

func TestRecordHashStableSansBufferUID(t *testing.T) {
	t.Parallel()
	a := ringRec(1, 1000, "hi")
	b := *a
	b.Buffer = "system"
	b.UID = 1000
	if RecordHash(a) != RecordHash(&b) {
		t.Fatal("Buffer/UID must not affect RecordHash (#259 Q6)")
	}
	c := *a
	c.Msg = "other"
	if RecordHash(a) == RecordHash(&c) {
		t.Fatal("Msg must affect RecordHash")
	}
}

func TestResumeRingObserveCapAndTake(t *testing.T) {
	t.Parallel()
	r := NewResumeRing("boot-1", 3)
	r.Observe(1, 11)
	r.Observe(2, 22)
	r.Observe(3, 33)
	if r.Len() != 3 || r.Count(11) != 1 || r.Count(22) != 1 || r.Count(33) != 1 {
		t.Fatalf("full: len=%d counts=%v", r.Len(), r.Counts())
	}
	r.Observe(4, 44) // drops 11
	if r.Len() != 3 || r.Count(11) != 0 || r.Count(44) != 1 {
		t.Fatalf("rolled: len=%d counts=%v", r.Len(), r.Counts())
	}
	if !r.Take(22) || r.Take(22) {
		t.Fatal("Take once")
	}
	r.Observe(5, 22) // duplicate hash ok (multiset)
	if r.Count(22) != 1 {
		t.Fatalf("re-observe after take: %d", r.Count(22))
	}
}

func TestResumeRingResetClearsBoot(t *testing.T) {
	t.Parallel()
	r := NewResumeRing("a", 10)
	r.Observe(1, 99)
	r.Reset("b")
	if r.Boot() != "b" || r.Len() != 0 || r.Count(99) != 0 {
		t.Fatalf("reset: boot=%s len=%d count=%d", r.Boot(), r.Len(), r.Count(99))
	}
}

func TestResumeRingObserveRecordSplitRuns(t *testing.T) {
	t.Parallel()
	r := NewResumeRing("boot", 100)
	ns := int64(1e15)
	r.ObserveRecord(ringRec(1, ns, "part1"))
	r.ObserveRecord(ringRec(2, ns, "part2"))
	r.ObserveRecord(ringRec(3, ns, "part3"))
	r.ObserveRecord(ringRec(4, ns+1000, "other")) // closes the run
	whole := ringRec(0, ns, "part1\npart2\npart3")
	if r.Count(RecordHash(whole)) != 1 {
		t.Fatalf("whole-entry hash missing: counts=%v", r.Counts())
	}
	if r.Count(RecordHash(ringRec(0, ns, "part1"))) != 1 ||
		r.Count(RecordHash(ringRec(0, ns, "part2"))) != 1 ||
		r.Count(RecordHash(ringRec(0, ns, "part3"))) != 1 {
		t.Fatalf("pieces: %v", r.Counts())
	}
	if r.Count(RecordHash(ringRec(0, ns+1000, "other"))) != 1 {
		t.Fatal("other")
	}
	if r.Len() != 4 {
		t.Fatalf("len=%d", r.Len())
	}
}

func TestRebuildResumeRingMatchesTailWindow(t *testing.T) {
	t.Parallel()
	st := openRingStore(t)
	seedBoot(st, "boot-1", 1)
	ns := time.Now().Add(-time.Minute).UnixNano() / 1000 * 1000
	var recs []*model.Record
	for i := 0; i < 10; i++ {
		recs = append(recs, ringRec(0, ns+int64(i)*1000, fmt.Sprintf("m%d", i)))
	}
	// split multi-line run in the middle (same header)
	recs = append(recs,
		ringRec(0, ns+20_000, "p1"),
		ringRec(0, ns+20_000, "p2"),
		ringRec(0, ns+20_000, "p3"),
		ringRec(0, ns+30_000, "tail"),
	)
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	ring := RebuildResumeRing(st, 100)
	if ring.Boot() != "boot-1" {
		t.Fatalf("boot %q", ring.Boot())
	}
	if ring.Len() != len(recs) {
		t.Fatalf("len=%d want %d", ring.Len(), len(recs))
	}
	whole := ringRec(0, ns+20_000, "p1\np2\np3")
	if ring.Count(RecordHash(whole)) != 1 {
		t.Fatal("rebuild must count split whole-entry hash (#192)")
	}
	for _, r := range recs {
		if ring.Count(RecordHash(r)) < 1 {
			t.Fatalf("missing %q", r.Msg)
		}
	}
	// cap: only last 5 arrivals (+ possible split extras that fall in window)
	small := RebuildResumeRing(st, 5)
	if small.Len() != 5 {
		t.Fatalf("cap len=%d", small.Len())
	}
	// oldest of the 14 should be gone from arrival slots; first msg "m0" out
	if small.Count(RecordHash(ringRec(0, ns, "m0"))) != 0 {
		t.Fatal("cap should drop oldest arrivals")
	}
	if small.Count(RecordHash(ringRec(0, ns+30_000, "tail"))) != 1 {
		t.Fatal("newest must remain")
	}
}

func TestRebuildResumeRingIgnoresPriorBoot(t *testing.T) {
	t.Parallel()
	st := openRingStore(t)
	ns := int64(1e15)
	if err := st.Append([]*model.Record{ringRec(0, ns, "old-boot")}); err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *Meta) {
		m.LastBootID = "boot-old"
		m.Sessions = []Session{{BootID: "boot-old", Boot: "old", FirstSeq: 1}}
	})
	// new session starting at seq 2
	st.UpdateMeta(func(m *Meta) {
		m.LastBootID = "boot-new"
		m.Sessions = []Session{
			{BootID: "boot-old", Boot: "old", FirstSeq: 1},
			{BootID: "boot-new", Boot: "new", FirstSeq: 2},
		}
	})
	if err := st.Append([]*model.Record{ringRec(0, ns+1000, "new-boot")}); err != nil {
		t.Fatal(err)
	}
	ring := RebuildResumeRing(st, 100)
	if ring.Boot() != "boot-new" {
		t.Fatalf("boot %q", ring.Boot())
	}
	if ring.Count(RecordHash(ringRec(0, ns, "old-boot"))) != 0 {
		t.Fatal("prior boot line must not be in ring")
	}
	if ring.Count(RecordHash(ringRec(0, ns+1000, "new-boot"))) != 1 {
		t.Fatal("current boot line missing")
	}
}

func TestResumeRingMergeExportAndSnapshot(t *testing.T) {
	t.Parallel()
	r := NewResumeRing("b", 10)
	r.Observe(1, 7)
	r.MergeExport([]uint64{7, 9})
	if r.Count(7) != 2 || r.Count(9) != 1 {
		t.Fatalf("merge: %v", r.Counts())
	}
	if r.Len() != 1 {
		t.Fatalf("merge must not grow deque: %d", r.Len())
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len %d", len(snap))
	}
}
