package device

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Phase A of #259: RebuildResumeRing multiset must equal tailMultiset
// (including splitRuns). recordOnce is unchanged until Phase B.

func equivRec(ns int64, pid int, tag, msg string) *model.Record {
	return &model.Record{
		TimeNs: ns, PID: pid, TID: pid, Level: 'I', Tag: tag, Msg: msg,
		Boot: "b", UID: -1,
	}
}

func openEquivStore(t *testing.T, boot string) *store.Store {
	t.Helper()
	st, err := store.OpenDir(filepath.Join(t.TempDir(), "dev"), "dev", store.Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.UpdateMeta(func(m *store.Meta) {
		m.LastBootID = boot
		m.Sessions = []store.Session{{BootID: boot, Boot: boot, FirstSeq: 1}}
	})
	return st
}

func sameMultiset(t *testing.T, name string, want multiset, got map[uint64]int32) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: key count want %d got %d\nwant=%v\ngot=%v", name, len(want), len(got), want, got)
	}
	for h, n := range want {
		if got[h] != n {
			t.Fatalf("%s: hash %d want %d got %d", name, h, n, got[h])
		}
	}
}

func TestRebuildResumeRingEqualsTailMultiset(t *testing.T) {
	t.Parallel()
	st := openEquivStore(t, "boot-eq")
	ns := time.Now().Add(-2 * time.Minute).UnixNano() / 1000 * 1000
	recs := []*model.Record{
		equivRec(ns, 100, "A", "single"),
		equivRec(ns+1000, 100, "ML", "part1"),
		equivRec(ns+1000, 100, "ML", "part2"),
		equivRec(ns+1000, 100, "ML", "part3"),
		equivRec(ns+2000, 101, "B", "other"),
		equivRec(ns+3000, 100, "A", "dup"),
		equivRec(ns+3000, 102, "X", "between"),
		equivRec(ns+3000, 100, "A", "dup"), // same µs twice (#52 multiset)
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{100, 5, 3, 1, store.DefaultResumeRingCap} {
		want := tailMultiset(st, n)
		ring := store.RebuildResumeRing(st, n)
		sameMultiset(t, fmt.Sprintf("n=%d", n), want, ring.Counts())
		// RecordHash must match device.recHash for every stored line
		st.Scan(0, st.LastSeq()+1, false, func(r *model.Record) bool {
			if store.RecordHash(r) != recHash(r) {
				t.Fatalf("RecordHash != recHash for %q", r.Msg)
			}
			return true
		})
	}
}

func TestRebuildResumeRingEqualsTailAcrossSessions(t *testing.T) {
	t.Parallel()
	st := openEquivStore(t, "boot-old")
	ns := int64(1e15)
	if err := st.Append([]*model.Record{equivRec(ns, 1, "T", "old")}); err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.LastBootID = "boot-new"
		m.Sessions = []store.Session{
			{BootID: "boot-old", Boot: "old", FirstSeq: 1},
			{BootID: "boot-new", Boot: "new", FirstSeq: 2},
		}
	})
	if err := st.Append([]*model.Record{
		equivRec(ns+1000, 1, "T", "a"),
		equivRec(ns+1000, 1, "T", "b"), // split potential
		equivRec(ns+2000, 2, "U", "c"),
	}); err != nil {
		t.Fatal(err)
	}
	want := tailMultiset(st, 100)
	ring := store.RebuildResumeRing(st, 100)
	sameMultiset(t, "cross-boot", want, ring.Counts())
	if ring.Boot() != "boot-new" {
		t.Fatalf("boot %q", ring.Boot())
	}
}

// Documents #259 answer 7: the session-tail ring (≡ tailMultiset) is a
// broader -T compare set than today's boundaryMultiset (10s slack). Phase B
// will switch -T resumes onto the ring; Phase A only pins the property so
// the intentional broadening stays tested.
func TestResumeRingBroaderThanBoundarySlack(t *testing.T) {
	t.Parallel()
	st := openEquivStore(t, "boot-slack")
	arrNs := time.Now().UnixNano() / 1000 * 1000
	oldNs := arrNs - int64(30*time.Second) // older than resumeSlack (10s)
	nearNs := arrNs - int64(2*time.Second)
	recs := []*model.Record{
		equivRec(oldNs, 10, "OLD", "outside-slack"),
		equivRec(nearNs, 11, "NEAR", "inside-slack"),
		equivRec(arrNs, 12, "NEW", "arrival"),
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) { m.ArrivalTimeNs = arrNs })

	boundary := boundaryMultiset(st, arrNs)
	ring := store.RebuildResumeRing(st, 100)
	oldH := recHash(recs[0])
	nearH := recHash(recs[1])

	if boundary.take(oldH) {
		t.Fatal("boundaryMultiset should exclude the >10s-old line")
	}
	if !boundary.take(nearH) {
		t.Fatal("boundaryMultiset should include the near line")
	}
	if ring.Count(store.RecordHash(recs[0])) != 1 {
		t.Fatal("ring must include the >10s-old line (broader -T compare set)")
	}
	if ring.Count(store.RecordHash(recs[1])) != 1 {
		t.Fatal("ring must include the near line")
	}
}
