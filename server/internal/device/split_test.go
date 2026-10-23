package device

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// A multi-line entry that the live assembler flushed in pieces (issue #192)
// is recognised when the replay after a reconnect assembles it whole.

func splitRec(ns int64, msg string) *model.Record {
	return &model.Record{TimeNs: ns, PID: 100, TID: 100, Level: 'I', Tag: "ML", Msg: msg, Boot: "b", UID: -1}
}

// Deterministic: the multisets built from the store contain the hash of the
// whole entry for a run of records with one header, and nothing extra for
// records whose headers differ.
func TestResumeMultisetsCountSplitEntries(t *testing.T) {
	t.Parallel()
	st, err := store.OpenDir(filepath.Join(t.TempDir(), "dev"), "dev", store.Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ns := time.Now().Add(-time.Minute).UnixNano() / 1000 * 1000
	recs := []*model.Record{
		splitRec(ns, "single"),
		splitRec(ns+1000, "part1"), splitRec(ns+1000, "part2"), splitRec(ns+1000, "part3"), // split in three
		splitRec(ns+2000, "other"),
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	whole := splitRec(ns+1000, "part1\npart2\npart3")
	// #259: ResumeRing (≡ tailMultiset) is the only compare set; boundaryMultiset gone.
	for name, set := range map[string]multiset{
		"tail": tailMultiset(st, 100),
		"ring": multiset(store.RebuildResumeRing(st, 100).Counts()),
	} {
		if !set.take(recHash(whole)) {
			t.Errorf("%s: the replayed whole entry is not recognised", name)
		}
		if set.take(recHash(whole)) {
			t.Errorf("%s: the whole entry counted twice", name)
		}
		for _, r := range recs { // pieces are still known
			if !set.take(recHash(r)) {
				t.Errorf("%s: stored %q lost", name, r.Msg)
			}
		}
		if len(set) != 6 { // 5 stored + the whole entry (all taken: counts are 0 but keys stay)
			t.Errorf("%s: %d keys", name, len(set))
		}
	}
	// different headers never join
	st2, _ := store.OpenDir(filepath.Join(t.TempDir(), "dev2"), "dev", store.Config{}, true)
	defer st2.Close()
	st2.Append([]*model.Record{splitRec(ns, "a"), splitRec(ns+1000, "b")})
	if set := tailMultiset(st2, 10); len(set) != 2 {
		t.Errorf("records with different headers joined: %d keys", len(set))
	}
}

// End to end (probe zz_blind_split_device_test): a slow transport splits the
// entry, the reconnect replay must not store it a third time.
func TestSplitMultilineMessageIsNotDuplicatedOnResume(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	at := time.Now().Add(-time.Second)
	e.Log("FAKEA", fakeadb.Line(at, 100, "ML", "part1"), fakeadb.Line(at, 100, "ML", "part2"))
	e.SetDelay("FAKEA", 600*time.Millisecond) // slow transport: gap between physical lines
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	count := func() (all []string) {
		for _, s := range messages(d.Store) {
			if strings.Contains(s, "part") {
				all = append(all, s)
			}
		}
		return
	}
	waitFor(t, "both pieces stored", 30*time.Second, func() bool { return len(count()) >= 2 && !d.CatchUp().Pending })
	before := count()
	reconnect(t, e, m, "FAKEA")
	e.SetDelay("FAKEA", 0)
	d, _ = resume(t, e, m, "FAKEA")
	time.Sleep(500 * time.Millisecond)
	after := count()
	if strings.Join(after, "|") != strings.Join(before, "|") {
		t.Errorf("the resume changed the stored lines: before %q, after %q", before, after)
	}
}
