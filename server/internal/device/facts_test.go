package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #225, PR1: persisted open-fact lookup, the single attach function
// under crashMu, evidence identity (idempotency), incarnation binding.

const factT0 = int64(1_700_000_000e9)

type factRig struct {
	t    *testing.T
	dir  string
	st   *store.Store
	p    *Pipeline
	boot string
}

func newFactRig(t *testing.T) *factRig {
	t.Helper()
	r := &factRig{t: t, dir: t.TempDir(), boot: "b1"}
	st, err := store.Open(r.dir, "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.UpdateMeta(func(m *store.Meta) {
		m.Sessions = []store.Session{{BootID: "b1-0", Boot: "b1", FirstSeq: 1}}
		m.LastBootID = "b1-0"
	})
	r.st = st
	r.restart()
	return r
}

// restart is a new service process over the same directory: a new
// pipeline (detector memory and process table gone, reloaded from files).
func (r *factRig) restart() {
	r.p = NewPipeline(r.st, r.boot, time.UTC)
	r.p.Delay = 0
}

func (r *factRig) ns(sec int64) int64 { return factT0 + sec*1e9 }

// feed stores and detects the records one batch at a time, in order.
func (r *factRig) feed(rs ...*model.Record) {
	r.t.Helper()
	now := time.Now()
	for _, x := range rs {
		r.p.Accept(x, now)
		if err := r.p.Drain(true, now); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *factRig) rec(sec int64, tag string, lvl model.Level, msg string) *model.Record {
	return &model.Record{TimeNs: r.ns(sec), PID: 1000, TID: 1000, Level: lvl, Tag: tag, Msg: msg, Boot: r.boot, UID: -1, Buffer: "system"}
}

func (r *factRig) procStart(sec int64, pid int, name string) *model.Record {
	return r.rec(sec, "am_proc_start", 'I', fmt.Sprintf("[0,%d,10001,%s,service,{%s/.S}]", pid, name, name))
}

func (r *factRig) amANR(sec int64, pid int, proc, reason string) *model.Record {
	return r.rec(sec, "am_anr", 'I', fmt.Sprintf("[0,%d,%s,0,%s]", pid, proc, reason))
}

func (r *factRig) anrBlock(sec int64, pid int, proc, reason string) *model.Record {
	return r.rec(sec, "ActivityManager", model.Error, fmt.Sprintf("ANR in %s (%s/.Main)\nPID: %d\nReason: %s\nCPU usage from 0ms to 1000ms:\n  10%% 1/x: 5%% user", proc, proc, pid, reason))
}

func (r *factRig) dropboxANR(sec int64, pid int, proc, reason string) {
	ts := time.Unix(0, r.ns(sec)).UTC().Format("2006-01-02 15:04:05")
	r.p.ApplyDropbox("Drop box contents\n========================================\n" + ts + " data_app_anr (text, 100 bytes)\nProcess: " + proc +
		"\nPID: " + fmt.Sprint(pid) + "\nSubject: " + reason + "\n\n----- pid " + fmt.Sprint(pid) + " at (no time) -----\nCmd line: " + proc + "\nDump-at: " + fmt.Sprint(sec) + "\n\n")
}

func (r *factRig) facts() []*analysis.Crash {
	cs := LoadCrashes(r.st)
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs // oldest first
}

// versions are the stored lines of one id, oldest first.
func (r *factRig) versions(id string) []*analysis.Crash {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.st.Dir, "crashes.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []*analysis.Crash
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		c := new(analysis.Crash)
		if json.Unmarshal([]byte(ln), c) != nil {
			r.t.Fatalf("bad line %q", ln)
		}
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

func (r *factRig) lifetime(pid int, sec int64, seq uint64) string {
	r.t.Helper()
	p := r.p.Procs.LookupSeq(pid, r.ns(sec), seq)
	if p == nil {
		r.t.Fatalf("no lifetime for pid %d", pid)
	}
	return p.ID
}

const reasonA = "Input dispatching timed out (Waited 5001ms for MotionEvent)"

// §1.3 / test plan 7: for every order of the three sources of one ANR and
// every restart of the service process between them, one fact.
func TestANRRestartBetweenSources(t *testing.T) {
	t.Parallel()
	steps := []string{"event", "block", "dropbox"}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		for mask := 0; mask < 4; mask++ { // restart before step 2 (bit 0) and step 3 (bit 1)
			name := fmt.Sprintf("%s-%s-%s/restart%02b", steps[perm[0]], steps[perm[1]], steps[perm[2]], mask)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newFactRig(t)
				r.feed(r.procStart(10, 4321, "com.foo"))
				for i, s := range perm {
					if i > 0 && mask>>(i-1)&1 == 1 {
						r.restart()
					}
					switch s {
					case 0:
						r.feed(r.amANR(60, 4321, "com.foo", reasonA))
					case 1:
						r.feed(r.anrBlock(100, 4321, "com.foo", reasonA))
					case 2:
						r.dropboxANR(105, 4321, "com.foo", reasonA)
					}
				}
				fs := r.facts()
				if len(fs) != 1 {
					for _, f := range fs {
						t.Logf("%s sources=%v inc=%s", f.ID, f.Sources, f.Inc)
					}
					t.Fatalf("%d facts, want 1", len(fs))
				}
				f := fs[0]
				for _, s := range []string{"events:am_anr", "logcat:system", "dropbox:data_app_anr"} {
					if !hasSource(f, s) {
						t.Errorf("missing %s: %v", s, f.Sources)
					}
				}
				if f.Inc == "" || f.IncWeak {
					t.Errorf("inc %q weak=%v", f.Inc, f.IncWeak)
				}
			})
		}
	}
}

// The two sources of one ANR pair at any gap up to the veto cap
// (anrEvidenceCap, 10 min), with or without a restart between them, and not
// beyond it (PR2 of #225 replaced the 120 s window).
func TestANRGapUpToEvidenceCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		gap     int64
		restart bool
		want    int
	}{{5, false, 1}, {5, true, 1}, {120, true, 1}, {130, false, 1}, {130, true, 1}, {300, true, 1}, {599, false, 1}, {600, true, 1}, {601, false, 2}, {601, true, 2}} {
		t.Run(fmt.Sprintf("gap%d-restart%v", tc.gap, tc.restart), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(1000, 4321, "com.foo", reasonA))
			if tc.restart {
				r.restart()
			}
			r.feed(r.anrBlock(1000+tc.gap, 4321, "com.foo", reasonA))
			if n := len(r.facts()); n != tc.want {
				t.Fatalf("%d facts, want %d", n, tc.want)
			}
		})
	}
}

// Idempotency: the same records fed again by a new detector (after a
// restart), the same DropBox output fetched again, change nothing: no new
// fact and no new line.
func TestEvidenceRepeatIsNoOpAfterRestart(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	ev, bl := r.amANR(60, 4321, "com.foo", reasonA), r.anrBlock(70, 4321, "com.foo", reasonA)
	r.feed(r.procStart(10, 4321, "com.foo"), ev, bl)
	r.dropboxANR(75, 4321, "com.foo", reasonA)
	lines := func() int {
		b, _ := os.ReadFile(filepath.Join(r.st.Dir, "crashes.jsonl"))
		return strings.Count(string(b), "\n")
	}
	n0, f0 := lines(), r.facts()
	if len(f0) != 1 || len(f0[0].Ev) != 3 {
		t.Fatalf("setup: %+v", f0)
	}
	r.restart()
	// the same records again, same seqs (a replay): the new detector knows nothing
	for _, x := range []*model.Record{ev, bl} {
		cp := *x
		cp.Seq = 0
		r.p.Det.Feed(&model.Record{TimeNs: cp.TimeNs, PID: cp.PID, TID: cp.TID, Level: cp.Level, Tag: cp.Tag, Msg: cp.Msg, Boot: cp.Boot, Buffer: cp.Buffer, Seq: x.Seq})
	}
	if lines() != n0 || len(r.facts()) != 1 {
		t.Fatalf("replay changed the file: %d -> %d lines, %d facts", n0, lines(), len(r.facts()))
	}
	// the same DropBox output again, with its dropbox_seen keys gone
	r.st.CompactAux("dropbox_seen", func([][]byte) [][]byte { return nil })
	r.dropboxANR(75, 4321, "com.foo", reasonA)
	if lines() != n0 || len(r.facts()) != 1 {
		t.Fatalf("DropBox refetch changed the file: %d -> %d lines", n0, lines())
	}
}

// ---- incarnation binding (design 3.1.1, tests 4d-4f) ----

func incs(vs []*analysis.Crash) []string {
	var out []string
	for _, v := range vs {
		out = append(out, fmt.Sprintf("%s/%v", v.Inc, v.IncWeak))
	}
	return out
}

// 4d: am_anr resolves to the previous lifetime of the pid, the start line of
// the new one arrives (device time before the evidence), then the block: one
// fact, inc re-bound once from the old to the new lifetime; also with a
// restart of the service process between the steps.
func TestIncRebindWhenStartLineArrivesLate(t *testing.T) {
	t.Parallel()
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("restart%03b", mask), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 100, "com.foo"))
			old := r.lifetime(100, 10, 1)
			r.feed(r.amANR(60, 100, "com.foo", reasonA)) // seq 2
			if mask&1 != 0 {
				r.restart()
			}
			r.feed(r.procStart(58, 100, "com.foo")) // seq 3: device time before the evidence
			if mask&2 != 0 {
				r.restart()
			}
			r.feed(r.anrBlock(70, 100, "com.foo", reasonA)) // seq 4
			if mask&4 != 0 {
				r.restart()
			}
			fs := r.facts()
			if len(fs) != 1 {
				t.Fatalf("%d facts, want 1", len(fs))
			}
			neu := r.lifetime(100, 70, 4)
			if neu == old {
				t.Fatal("setup: no second lifetime")
			}
			vs := r.versions(fs[0].ID)
			if vs[0].Inc != old || vs[0].IncWeak {
				t.Errorf("first version bound to %s, want the previous lifetime %s", vs[0].Inc, old)
			}
			last := vs[len(vs)-1]
			if last.Inc != neu || last.IncWeak {
				t.Errorf("final inc %s weak=%v, want %s", last.Inc, last.IncWeak, neu)
			}
			changes := 0
			for i := 1; i < len(vs); i++ {
				if vs[i].Inc != vs[i-1].Inc {
					changes++
				}
			}
			if changes != 1 {
				t.Errorf("inc re-bound %d times (%v), want exactly once", changes, incs(vs))
			}
			// a later source does not move it again
			r.dropboxANR(75, 100, "com.foo", reasonA)
			if vs := r.versions(fs[0].ID); vs[len(vs)-1].Inc != neu {
				t.Errorf("inc moved after completion: %v", incs(vs))
			}
		})
	}
}

// 4e: the start line of the next lifetime is more than the 2 s slack after
// the evidence: the two sides resolve to different lifetimes, two facts, no
// re-bind of the first one.
func TestIncReuseBeyondSlackSplitsFacts(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart%v", restart), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 100, "com.foo"))
			old := r.lifetime(100, 10, 1)
			r.feed(r.amANR(60, 100, "com.foo", reasonA))
			r.feed(r.procStart(75, 100, "com.foo")) // 15 s after the evidence: a real reuse
			if restart {
				r.restart()
			}
			r.feed(r.anrBlock(80, 100, "com.foo", reasonA))
			fs := r.facts()
			if len(fs) != 2 {
				t.Fatalf("%d facts, want 2 (different lifetimes)", len(fs))
			}
			if vs := r.versions(fs[0].ID); len(vs) != 1 || vs[0].Inc != old {
				t.Errorf("first fact re-bound or rewritten: %v", incs(vs))
			}
			if fs[1].Inc == old || fs[1].IncWeak {
				t.Errorf("second fact inc %s weak=%v", fs[1].Inc, fs[1].IncWeak)
			}
		})
	}
}

// A complete fact is never re-bound.
func TestIncCompleteFactNotRebound(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 100, "com.foo"))
	old := r.lifetime(100, 10, 1)
	r.feed(r.amANR(60, 100, "com.foo", reasonA), r.anrBlock(65, 100, "com.foo", reasonA))
	r.dropboxANR(66, 100, "com.foo", reasonA)
	r.feed(r.procStart(59, 100, "com.foo")) // would now be the anchor's lifetime
	fs := r.facts()
	if len(fs) != 1 {
		t.Fatalf("%d facts", len(fs))
	}
	r.feed(r.rec(80, "x", 'I', "unrelated"))
	vs := r.versions(fs[0].ID)
	for _, v := range vs {
		if v.Inc != old {
			t.Fatalf("complete fact re-bound: %v", incs(vs))
		}
	}
}

// 4f: no lifetime known at creation: weak token; upgraded when the start line
// arrives; never to an incompatible process name.
func TestIncWeakFactUpgrade(t *testing.T) {
	t.Parallel()
	t.Run("upgraded", func(t *testing.T) {
		t.Parallel()
		r := newFactRig(t)
		r.feed(r.amANR(60, 100, "com.foo", reasonA)) // seq 1, pid unknown
		fs := r.facts()
		if len(fs) != 1 || !fs[0].IncWeak || fs[0].Inc != "p100@1" {
			t.Fatalf("want a weak p100@1 fact, got %+v", fs)
		}
		r.restart()
		r.feed(r.procStart(58, 100, "com.foo"))
		r.feed(r.anrBlock(70, 100, "com.foo", reasonA))
		fs = r.facts()
		if len(fs) != 1 {
			t.Fatalf("%d facts", len(fs))
		}
		if want := r.lifetime(100, 70, 3); fs[0].Inc != want || fs[0].IncWeak {
			t.Errorf("inc %s weak=%v, want %s", fs[0].Inc, fs[0].IncWeak, want)
		}
	})
	t.Run("never to an incompatible name", func(t *testing.T) {
		t.Parallel()
		r := newFactRig(t)
		r.feed(r.amANR(60, 100, "com.foo", reasonA))
		r.feed(r.procStart(58, 100, "org.other"))
		r.feed(r.anrBlock(70, 100, "com.foo", reasonA))
		for _, f := range r.facts() {
			if !f.IncWeak || !strings.HasPrefix(f.Inc, "p100@") {
				t.Errorf("%s upgraded to %s (weak=%v) although the lifetime is org.other", f.ID, f.Inc, f.IncWeak)
			}
		}
	})
	t.Run("weak facts pair by name and pid", func(t *testing.T) {
		t.Parallel()
		r := newFactRig(t)
		r.feed(r.amANR(60, 100, "com.foo", reasonA), r.anrBlock(70, 100, "com.foo", reasonA))
		fs := r.facts()
		if len(fs) != 1 || !fs[0].IncWeak {
			t.Fatalf("%+v", fs)
		}
		// a different process of the same pid number does not pair
		r.feed(r.amANR(300, 100, "com.bar", reasonA), r.anrBlock(310, 100, "com.bar", reasonA))
		if n := len(r.facts()); n != 2 {
			t.Fatalf("%d facts, want 2", n)
		}
	})
}

// ---- Proc.ID stability (design 3.1.1, tests 4a-4c, 4g) ----

type idKey struct {
	pid int
	seq uint64
}

func (r *factRig) ids(keys []idKey) map[idKey]string {
	out := map[idKey]string{}
	for _, k := range keys {
		if p := r.p.Procs.LookupSeq(k.pid, 0, k.seq); p != nil {
			out[k] = p.ID
		}
	}
	return out
}

func lifetimeRecords(r *factRig) []*model.Record {
	return []*model.Record{
		r.procStart(10, 100, "com.foo"),                          // seq 1
		r.procStart(20, 200, "com.bar"),                          // seq 2
		r.rec(30, "am_proc_died", 'I', "[0,100,com.foo,900,19]"), // seq 3
		r.procStart(40, 100, "com.baz"),                          // seq 4: pid 100 reused
	}
}

var lifetimeKeys = []idKey{{100, 1}, {100, 2}, {200, 2}, {100, 4}, {100, 5}}

// 4a: identical before and after Restore (a pipeline over the same dir).
func TestProcIDsStableAcrossRestore(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(lifetimeRecords(r)...)
	before := r.ids(lifetimeKeys)
	if len(before) < 4 || before[idKey{100, 1}] == before[idKey{100, 4}] {
		t.Fatalf("setup: %v", before)
	}
	r.restart()
	if after := r.ids(lifetimeKeys); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("IDs changed across Restore:\nbefore %v\nafter  %v", before, after)
	}
	// and across a compaction of procs.jsonl
	CompactAux(r.st, time.Now())
	r.restart()
	if after := r.ids(lifetimeKeys); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("IDs changed across CompactAux:\nbefore %v\nafter  %v", before, after)
	}
}

// 4b: identical after set-root (CarryOver into a new root, new pipeline).
func TestProcIDsStableAcrossSetRoot(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(lifetimeRecords(r)...)
	before := r.ids(lifetimeKeys)
	r.st.Close()
	newRoot := filepath.Join(t.TempDir(), "new")
	cs, err := CarryOver(r.dir, newRoot)
	if err != nil || len(cs) != 1 {
		t.Fatalf("carry: %v %v", cs, err)
	}
	st, err := store.Open(newRoot, "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	r2 := &factRig{t: t, dir: newRoot, st: st, boot: "b1"}
	r2.restart()
	if after := r2.ids(lifetimeKeys); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("IDs changed across set-root:\nbefore %v\nafter  %v", before, after)
	}
}

// 4c: a ps-seeded entry merged with am_proc_start keeps its ID (one entry,
// StartNs set), also through a restart and CompactAux.
func TestProcIDStableAcrossPSSeedMerge(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.p.SeedPS("  PID   UID NAME\n  100 10001 com.foo\n", r.ns(5))
	seeded := r.p.Procs.Lookup(100, r.ns(5))
	if seeded == nil || seeded.StartNs != 0 || seeded.Source != "ps" {
		t.Fatalf("seed: %+v", seeded)
	}
	id := seeded.ID
	// the start line (device time just before the snapshot) arrives after it
	r.feed(r.procStart(4, 100, "com.foo"))
	check := func(when string) {
		t.Helper()
		got := r.p.Procs.Lookup(100, r.ns(6))
		if got == nil || got.ID != id || got.StartNs == 0 || got.Source != "am_proc_start" {
			t.Fatalf("%s: %+v, want ID %s merged with the start line", when, got, id)
		}
		n := 0
		for _, x := range LoadProcs(r.st) {
			if x.PID == 100 {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: %d entries for pid 100, want 1", when, n)
		}
	}
	check("after the merge")
	r.restart()
	check("after Restore")
	CompactAux(r.st, time.Now())
	r.restart()
	check("after CompactAux")
}

// 4g: lifetimes minted after a Restore never take the ID of a restored one.
func TestProcIDNoCollisionAfterRestore(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(lifetimeRecords(r)...)
	seen := map[string]bool{}
	for _, x := range LoadProcs(r.st) {
		seen[x.ID] = true
	}
	r.restart()
	// the same pid again and again, plus a ps seed, after the restore
	r.feed(r.rec(50, "am_proc_died", 'I', "[0,100,com.baz,900,19]"), r.procStart(60, 100, "com.foo"),
		r.rec(70, "am_proc_died", 'I', "[0,100,com.foo,900,19]"), r.procStart(80, 100, "com.foo"))
	r.p.SeedPS("  PID   UID NAME\n  300 10002 com.new\n", r.ns(90))
	all := LoadProcs(r.st)
	if len(all) < 6 {
		t.Fatalf("%d lifetimes", len(all))
	}
	ids := map[string]bool{}
	for _, x := range all {
		if ids[x.ID] {
			t.Errorf("ID %s twice", x.ID)
		}
		ids[x.ID] = true
	}
	for id := range seen {
		if !ids[id] {
			t.Errorf("restored lifetime %s lost", id)
		}
	}
}
