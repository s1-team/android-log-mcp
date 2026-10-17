package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #225 PR1: the attach function (design 3.2.1) and legacy facts (3.3).

func (r *factRig) snapshot(i int, withBlock bool) *analysis.Crash {
	c := &analysis.Crash{Kind: "anr", Boot: r.boot, TimeNs: r.ns(1000 * int64(i)), Seq: uint64(10*i + 1), PID: 5000 + i,
		Process: "com.foo", Package: "com.foo", Headline: reasonA}
	c.AddEvidence(analysis.Evidence{S: "events:am_anr", Q: c.Seq, T: c.TimeNs, CK: analysis.ContentKey("events", "com.foo", reasonA)})
	if withBlock {
		c.Body = "ANR in com.foo"
		c.AddEvidence(analysis.Evidence{S: "logcat:system", Q: c.Seq + 1, T: c.TimeNs + 5e9, CK: analysis.ContentKey("logcat", "com.foo", reasonA)})
	}
	return c
}

// The lost-update race of design 1.6.7: detector snapshots and DropBox
// attaches of many ANRs from two goroutines (and a compaction rebuilding the
// index in between). Run with -race -count=20: every id's newest line holds
// all three evidences, the evidence of a later line is a superset of an
// earlier one, and no ANR gets two ids.
func TestAttachConcurrentWritesKeepEveryEvidence(t *testing.T) {
	r := newFactRig(t)
	const n = 24
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the detector's emit path
		defer wg.Done()
		for i := 1; i <= n; i++ {
			c := r.snapshot(i, false)
			r.p.EmitCrash(c) // events
			c = r.snapshot(i, true)
			r.p.EmitCrash(c) // the same crash, now with the block
		}
	}()
	go func() { // the DropBox worker
		defer wg.Done()
		for i := 1; i <= n; i++ {
			r.dropboxANR(1000*int64(i)+10, 5000+i, "com.foo", reasonA)
		}
	}()
	go func() { // the index is rebuilt in between
		defer wg.Done()
		for i := 0; i < 5; i++ {
			CompactAux(r.st, time.Now())
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	// DropBox entries that raced ahead of their detector snapshot, or lost
	// to it on the one-to-one rule, are re-fetched like the worker would
	for i := 1; i <= n; i++ {
		r.dropboxANR(1000*int64(i)+10, 5000+i, "com.foo", reasonA)
	}
	facts := r.facts()
	byPID := map[int][]*analysis.Crash{}
	for _, f := range facts {
		byPID[f.PID] = append(byPID[f.PID], f)
	}
	for i := 1; i <= n; i++ {
		fs := byPID[5000+i]
		if len(fs) != 1 {
			t.Fatalf("pid %d: %d facts, want 1", 5000+i, len(fs))
		}
		f := fs[0]
		for _, s := range []string{"events:am_anr", "logcat:system", "dropbox:data_app_anr"} {
			if !hasSource(f, s) {
				t.Errorf("%s: newest line lacks %s (sources %v)", f.ID, s, f.Sources)
			}
		}
		if len(f.Ev) != 3 {
			t.Errorf("%s: %d evidences, want 3", f.ID, len(f.Ev))
		}
		var prev map[string]bool
		for _, v := range r.versions(f.ID) {
			cur := map[string]bool{}
			for _, e := range v.Ev {
				cur[e.S] = true
			}
			for s := range prev {
				if !cur[s] {
					t.Errorf("%s: evidence %s dropped by a later line", f.ID, s)
				}
			}
			prev = cur
		}
	}
	if len(facts) != n {
		t.Errorf("%d facts, want %d", len(facts), n)
	}
}

// A detector update made after the DropBox candidate was read and before
// the DropBox line was written is no longer lost: the attach re-reads the
// latest version under crashMu. (Both orders give the same newest line.)
func TestAttachKeepsDetectorUpdateAroundDropbox(t *testing.T) {
	t.Parallel()
	for _, dropFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("dropFirst=%v", dropFirst), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			c := r.snapshot(1, false)
			r.p.EmitCrash(c)
			if dropFirst {
				r.dropboxANR(1010, 5001, "com.foo", reasonA)
				c.Body = "ANR in com.foo"
				c.AddEvidence(analysis.Evidence{S: "logcat:system", Q: 12, T: c.TimeNs + 5e9, CK: "x"})
				r.p.EmitCrash(c) // the detector's copy never saw the DropBox merge
			} else {
				c.Body = "ANR in com.foo"
				c.AddEvidence(analysis.Evidence{S: "logcat:system", Q: 12, T: c.TimeNs + 5e9, CK: "x"})
				r.p.EmitCrash(c)
				r.dropboxANR(1010, 5001, "com.foo", reasonA)
			}
			fs := r.facts()
			if len(fs) != 1 || len(fs[0].Ev) != 3 || fs[0].Traces == "" || fs[0].Body == "" {
				t.Fatalf("%+v", fs)
			}
		})
	}
}

// Documented reproduction of the lost update with the merge order before
// this change (read and merge outside crashMu, then append the merged copy):
// skipped by default, not a failing test.
func TestOldMergeOrderLosesTheDetectorUpdate(t *testing.T) {
	if os.Getenv("ALM_REPRO_LOST_UPDATE") == "" {
		t.Skip("documentation of the pre-#225 behaviour; set ALM_REPRO_LOST_UPDATE=1 to run")
	}
	r := newFactRig(t)
	c := r.snapshot(1, false)
	r.p.EmitCrash(c)
	// old ApplyDropbox: read and merge outside the lock
	e := analysis.ParseDropbox("Drop box contents\n========================================\n"+
		time.Unix(0, r.ns(1010)).UTC().Format("2006-01-02 15:04:05")+" data_app_anr (text, 1 bytes)\nProcess: com.foo\nPID: 5001\nSubject: "+reasonA+
		"\n\n----- pid 5001 at (no time) -----\nCmd line: com.foo\n\n", time.UTC)
	full := CrashFull(r.st, c.ID)[0]
	merged, _ := analysis.MatchDropboxBoot([]*analysis.Crash{full}, e[0], r.boot, nil)
	// the detector update lands in between
	c.AddEvidence(analysis.Evidence{S: "logcat:system", Q: 12, T: c.TimeNs + 5e9, CK: "x"})
	r.p.EmitCrash(c)
	// old EmitCrash of the merged copy: KeepDropbox, then append as is
	r.st.AppendAux("crashes", crashAuxLine{Crash: merged, HostNs: time.Now().UnixNano()})
	if f := CrashFull(r.st, c.ID)[0]; !hasSource(f, "logcat:system") {
		t.Logf("lost: the newest line lacks logcat:system: %v", f.Sources)
	} else {
		t.Fatal("not reproduced")
	}
}

// ---- legacy facts and old-binary round trip (design 3.3, tests 8 and 14) ----

// oldCrash is analysis.Crash as 0.9.x declares it: no inc, incw or ev.
type oldCrash struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Boot      string   `json:"boot"`
	TimeNs    int64    `json:"t"`
	Seq       uint64   `json:"seq,omitempty"`
	PID       int      `json:"pid"`
	Process   string   `json:"process,omitempty"`
	Package   string   `json:"package,omitempty"`
	Thread    string   `json:"thread,omitempty"`
	Headline  string   `json:"headline"`
	Body      string   `json:"body,omitempty"`
	Signature string   `json:"sig,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Traces    string   `json:"traces,omitempty"`
	CPU       string   `json:"cpu,omitempty"`
	Dropbox   string   `json:"dropbox,omitempty"`
	Tombstone string   `json:"tombstone,omitempty"`
	Location  string   `json:"location,omitempty"`
}

type oldAuxLine struct {
	oldCrash
	HostNs int64 `json:"ht,omitempty"`
}

// oldBinaryUpdate is what a 0.9.x binary does with a fact it updates: decode
// the newest line into its own struct (dropping inc/ev) and append it.
func (r *factRig) oldBinaryUpdate(id string, mut func(*oldCrash)) {
	r.t.Helper()
	vs := r.versions(id)
	b, _ := json.Marshal(vs[len(vs)-1])
	var o oldCrash
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	if mut != nil {
		mut(&o)
	}
	r.st.AppendAux("crashes", oldAuxLine{oldCrash: o, HostNs: time.Now().UnixNano()})
}

func (r *factRig) legacyANR(id string, sec int64, pid int, sources ...string) {
	r.st.AppendAux("crashes", oldAuxLine{oldCrash: oldCrash{ID: id, Kind: "anr", Boot: r.boot, TimeNs: r.ns(sec), Seq: 1, PID: pid,
		Process: "com.foo", Package: "com.foo", Headline: reasonA, Sources: sources}, HostNs: time.Now().UnixNano()})
	r.st.UpdateMeta(func(m *store.Meta) { m.AnrSeq = max(m.AnrSeq, 1) })
}

// Item 14: a legacy fact (no ev) pairs with later evidence only inside the
// old windows kept as legacy constants; the first attach rewrites it with
// inc (weak: no lifetime) and synthesized ev.
func TestLegacyFactPairsInsideOldWindowsOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		gap  int64
		want int
	}{{100, 1}, {120, 1}, {121, 2}, {400, 2}} {
		t.Run(fmt.Sprintf("block+%ds", tc.gap), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.legacyANR("anr1", 1000, 4321, "events:am_anr")
			r.feed(r.anrBlock(1000+tc.gap, 4321, "com.foo", reasonA))
			fs := r.facts()
			if len(fs) != tc.want {
				t.Fatalf("%d facts, want %d", len(fs), tc.want)
			}
			vs := r.versions("anr1")
			if tc.want == 1 {
				last := vs[len(vs)-1]
				if len(last.Ev) != 2 || !last.IncWeak || last.Inc == "" || !hasSource(last, "logcat:system") || !hasSource(last, "events:am_anr") {
					t.Errorf("rewrite: %+v", last)
				}
				if last.Ev[0].S != "events:am_anr" || last.Ev[0].Q != 0 || last.Ev[0].CK != "" {
					t.Errorf("synthesized entry carries an identity: %+v", last.Ev[0])
				}
			} else if len(vs) != 1 || len(vs[0].Ev) != 0 {
				t.Errorf("legacy line touched: %d versions", len(vs))
			}
		})
	}
}

// A legacy fact that names all source classes is complete: a late block
// does not attach, and DropBox follows the old rules (taken ANR).
func TestLegacyCompleteFactTakesNothing(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.legacyANR("anr1", 1000, 4321, "events:am_anr", "logcat:system", "dropbox:data_app_anr")
	r.feed(r.anrBlock(1010, 4321, "com.foo", reasonA))
	r.feed(r.amANR(1020, 4321, "com.foo", reasonA))
	for _, f := range r.facts() {
		if f.ID == "anr1" && len(r.versions("anr1")) != 1 {
			t.Fatalf("complete legacy fact rewritten: %v", f.Sources)
		}
	}
	if len(r.facts()) < 2 {
		t.Fatalf("late sources attached to a complete fact")
	}
}

// Legacy DropBox pairing keeps the old rule and window.
func TestLegacyFactDropboxWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		gap  int64
		want int
	}{{100, 1}, {130, 2}} {
		r := newFactRig(t)
		r.legacyANR("anr1", 1000, 4321, "events:am_anr", "logcat:system")
		r.dropboxANR(1000+tc.gap, 4321, "com.foo", reasonA)
		if n := len(r.facts()); n != tc.want {
			t.Errorf("DropBox +%ds: %d facts, want %d", tc.gap, n, tc.want)
		}
	}
}

// Old-binary round trip: the new binary writes a fact, an old binary updates
// it (inc and ev dropped), the new binary reads it as legacy without error;
// a late source inside the legacy window attaches, one beyond it does not,
// and nothing is paired by ordinal or interval.
func TestOldBinaryRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gap  int64
		want int
	}{{"inside the window", 100, 1}, {"beyond the window", 130, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(1000, 4321, "com.foo", reasonA))
			if v := r.versions("anr1"); len(v) != 1 || len(v[0].Ev) != 1 || v[0].Inc == "" {
				t.Fatalf("new fact: %+v", v)
			}
			r.oldBinaryUpdate("anr1", func(o *oldCrash) { o.CPU = "cpu text from the old binary" })
			r.restart()
			f := LoadCrashes(r.st)
			if len(f) != 1 || len(f[0].Ev) != 0 || f[0].Inc != "" || f[0].CPU == "" {
				t.Fatalf("legacy read: %+v", f)
			}
			r.feed(r.anrBlock(1000+tc.gap, 4321, "com.foo", reasonA))
			if n := len(r.facts()); n != tc.want {
				t.Fatalf("%d facts, want %d", n, tc.want)
			}
			if tc.want == 1 {
				last := r.versions("anr1")
				l := last[len(last)-1]
				if len(l.Ev) != 2 || l.CPU == "" {
					t.Errorf("rewrite of the legacy fact: %+v", l)
				}
			}
		})
	}
}

// New reads old, no rewrite on read: opening a pipeline and listing the
// crashes of a legacy file changes no byte.
func TestLegacyFileNotRewrittenOnRead(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.legacyANR("anr1", 1000, 4321, "events:am_anr")
	path := filepath.Join(r.st.Dir, "crashes.jsonl")
	before, _ := os.ReadFile(path)
	r.restart()
	_ = LoadCrashes(r.st)
	_ = CrashSummaries(r.st)
	_ = OpenFacts(r.st, r.boot, "anr", 4321)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("the file changed on read:\n%s\n%s", before, after)
	}
}

// The index finds open facts by (boot, kind, pid) and keeps them current
// when a newer version of a fact is appended.
func TestOpenFactsIndex(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.amANR(100, 11, "com.a", reasonA), r.amANR(200, 22, "com.b", reasonA), r.amANR(1000, 11, "com.a", reasonA))
	got := OpenFacts(r.st, r.boot, "anr", 11)
	if len(got) != 2 || got[0].ID != "anr3" || got[1].ID != "anr1" {
		t.Fatalf("pid 11: %v", ids(got))
	}
	if n := len(OpenFacts(r.st, r.boot, "anr", 0)); n != 3 {
		t.Fatalf("pid 0 (all): %d", n)
	}
	if n := len(OpenFacts(r.st, "other-boot", "anr", 11)); n != 0 {
		t.Fatalf("other boot: %d", n)
	}
	r.feed(r.anrBlock(1010, 11, "com.a", reasonA)) // updates anr3 (a new version)
	got = OpenFacts(r.st, r.boot, "anr", 11)
	if len(got) != 2 || got[0].ID != "anr3" || !hasSource(got[0], "logcat:system") || len(got[0].Ev) != 2 || len(got[1].Ev) != 1 {
		t.Fatalf("after update: %s", ids(got))
	}
}

func ids(cs []*analysis.Crash) string {
	var s []string
	for _, c := range cs {
		s = append(s, c.ID)
	}
	return strings.Join(s, ",")
}

// CompactAux copies the newest line per id, so inc and ev survive it.
func TestCompactAuxKeepsIncAndEv(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(60, 4321, "com.foo", reasonA), r.anrBlock(70, 4321, "com.foo", reasonA))
	before := r.versions("anr1")
	last := before[len(before)-1]
	if len(before) < 2 || last.Inc == "" || len(last.Ev) != 2 {
		t.Fatalf("setup: %+v", before)
	}
	CompactAux(r.st, time.Now())
	after := r.versions("anr1")
	if len(after) != 1 || !reflect.DeepEqual(after[0].Ev, last.Ev) || after[0].Inc != last.Inc {
		t.Fatalf("after compaction: %+v", after)
	}
	r.restart()
	r.feed(r.rec(80, "x", 'I', "y"))
	if f := r.facts(); len(f) != 1 || len(f[0].Ev) != 2 {
		t.Fatalf("%+v", f)
	}
}
