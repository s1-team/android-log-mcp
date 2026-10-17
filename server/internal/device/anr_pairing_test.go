package device

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #225, PR2: ANR pairing by ordinal and interval (the window is a veto
// cap), the ordinal n written, DropBox compatibility, and the native
// crash whose sources arrive after the detector let go of it (#233).

const reasonB = "Broadcast of Intent { act=android.intent.action.BOOT_COMPLETED cmp=com.foo/.R }"

func anrFacts(r *factRig) []*analysis.Crash {
	var out []*analysis.Crash
	for _, f := range r.facts() {
		if f.Kind == "anr" {
			out = append(out, f)
		}
	}
	return out
}

func hasAll(f *analysis.Crash, ss ...string) bool {
	for _, s := range ss {
		if !hasSource(f, s) {
			return false
		}
	}
	return true
}

// Two real ANRs of one process lifetime, each with its own sources, are two
// facts (n = 1, 2) whatever the gap and wherever a restart falls.
func TestTwoANRsOfOneLifetimeNeverMerge(t *testing.T) {
	t.Parallel()
	for _, gap := range []int64{40, 130, 400} {
		for mask := 0; mask < 8; mask++ {
			t.Run(fmt.Sprintf("gap%d-restart%03b", gap, mask), func(t *testing.T) {
				t.Parallel()
				r := newFactRig(t)
				r.feed(r.procStart(10, 4321, "com.foo"))
				step := func(bit int) {
					if mask>>bit&1 == 1 {
						r.restart()
					}
				}
				r.feed(r.amANR(1000, 4321, "com.foo", reasonA))
				step(0)
				r.feed(r.anrBlock(1030, 4321, "com.foo", reasonA))
				r.dropboxANR(1035, 4321, "com.foo", reasonA)
				step(1)
				r.feed(r.amANR(1030+gap, 4321, "com.foo", reasonA))
				step(2)
				r.feed(r.anrBlock(1060+gap, 4321, "com.foo", reasonA))
				r.dropboxANR(1065+gap, 4321, "com.foo", reasonA)
				fs := anrFacts(r)
				if len(fs) != 2 {
					t.Fatalf("%d ANRs, want 2", len(fs))
				}
				for i, f := range fs {
					if !hasAll(f, "events:am_anr", "logcat:system", "dropbox:data_app_anr") {
						t.Errorf("%s sources %v", f.ID, f.Sources)
					}
					if f.N != i+1 || f.Inc == "" {
						t.Errorf("%s n=%d inc=%q, want n=%d", f.ID, f.N, f.Inc, i+1)
					}
				}
				if fs[0].Inc != fs[1].Inc {
					t.Errorf("one lifetime, two incs: %s %s", fs[0].Inc, fs[1].Inc)
				}
			})
		}
	}
}

// The ordinal restarts with a new lifetime of the same pid.
func TestOrdinalPerLifetime(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(100, 4321, "com.foo", reasonA), r.amANR(300, 4321, "com.foo", reasonA))
	r.restart()
	r.feed(r.procStart(400, 4321, "com.foo"), r.amANR(500, 4321, "com.foo", reasonA))
	fs := anrFacts(r)
	if len(fs) != 3 || fs[0].N != 1 || fs[1].N != 2 || fs[2].N != 1 {
		var ns []int
		for _, f := range fs {
			ns = append(ns, f.N)
		}
		t.Fatalf("ordinals %v, want [1 2 1]", ns)
	}
	// the ordinal is part of the stored line and survives the later sources
	r.feed(r.anrBlock(510, 4321, "com.foo", reasonA))
	for _, v := range r.versions(fs[2].ID) {
		if v.N != 1 {
			t.Errorf("version of %s: n=%d", fs[2].ID, v.N)
		}
	}
	if got := anrFacts(r); len(got) != 3 || !hasSource(got[2], "logcat:system") {
		t.Errorf("block did not attach to the newest ANR: %d facts", len(got))
	}
}

// A different reason is a different ANR, at any gap.
func TestANRDifferentReasonIsNewFact(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(100, 4321, "com.foo", reasonA))
	r.restart()
	r.feed(r.anrBlock(110, 4321, "com.foo", reasonB))
	if n := len(anrFacts(r)); n != 2 {
		t.Fatalf("%d ANRs, want 2", n)
	}
	// the same reason up to a suffix after " (" pairs
	r = newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(100, 4321, "com.foo", "Input dispatching timed out"))
	r.restart()
	r.feed(r.anrBlock(110, 4321, "com.foo", reasonA))
	if n := len(anrFacts(r)); n != 1 {
		t.Fatalf("prefix reason: %d ANRs, want 1", n)
	}
}

// A lost source: the am_anr of ANR 1 and the block of ANR 2 ... each source
// attaches to the latest ANR of the lifetime anchored before it.
func TestANRLostSourceInterval(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"),
		r.amANR(100, 4321, "com.foo", reasonA), // ANR 1: block and DropBox lost
		r.amANR(200, 4321, "com.foo", reasonA)) // ANR 2
	r.restart()
	r.feed(r.anrBlock(230, 4321, "com.foo", reasonA))
	fs := anrFacts(r)
	if len(fs) != 2 || hasSource(fs[0], "logcat:system") || !hasSource(fs[1], "logcat:system") {
		t.Fatalf("block of ANR 2 must go to ANR 2: %d facts", len(fs))
	}
	// the am_anr of ANR 3 is lost: its block meets ANR 2, which has a block
	// already: a new fact, not a second block on ANR 2
	r.feed(r.anrBlock(400, 4321, "com.foo", reasonA))
	if n := len(anrFacts(r)); n != 3 {
		t.Fatalf("%d ANRs, want 3", n)
	}
}

// DropBox entries of two ANRs, in both arrival orders, go to their own ANR.
func TestDropboxEntriesOfTwoANRsOutOfOrder(t *testing.T) {
	t.Parallel()
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(100, 4321, "com.foo", reasonA), r.amANR(200, 4321, "com.foo", reasonA))
			r.restart()
			at := []int64{105, 205}
			for _, i := range order {
				r.dropboxANR(at[i], 4321, "com.foo", reasonA)
			}
			fs := anrFacts(r)
			if len(fs) != 2 || !hasSource(fs[0], "dropbox:data_app_anr") || !hasSource(fs[1], "dropbox:data_app_anr") {
				t.Fatalf("%d facts: %v", len(fs), fs)
			}
		})
	}
}

// A DropBox entry with another reason does not attach to the open ANR.
func TestDropboxDifferentReasonStaysUnmatched(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.foo"), r.amANR(100, 4321, "com.foo", reasonA))
	r.dropboxANR(110, 4321, "com.foo", reasonB)
	for _, f := range anrFacts(r) {
		if f.ID == "anr1" && hasSource(f, "dropbox:data_app_anr") {
			t.Fatalf("attached to the ANR of another reason: %v", f.Sources)
		}
	}
}

// ---- native crash: the libc line, the tombstone and am_crash are three
// sources of one crash (#233) ----

func (r *factRig) libc(sec int64, pid int) *model.Record {
	x := r.rec(sec, "libc", model.Fatal, fmt.Sprintf("Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid %d (example.foo), pid %d (com.example.foo)", pid, pid))
	x.PID, x.Buffer = pid, "crash"
	return x
}

func (r *factRig) tombLines(sec int64, pid int) []*model.Record {
	var out []*model.Record
	for i, m := range []string{
		"*** *** *** *** *** *** *** *** *** *** *** *** *** *** *** ***",
		fmt.Sprintf("pid: %d, tid: %d, name: example.foo  >>> com.example.foo <<<", pid, pid),
		"signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000",
		"backtrace:",
		"      #00 pc 00000000000007fc  /data/app/x/lib/x86_64/liblogtest.so (f+12)",
	} {
		x := r.rec(sec, "DEBUG", model.Fatal, m)
		x.TimeNs += int64(i) * 1e6
		x.PID, x.Buffer = 6000, "crash"
		out = append(out, x)
	}
	return out
}

func (r *factRig) amCrashNative(sec int64, pid int) *model.Record {
	return r.rec(sec, "am_crash", 'I', fmt.Sprintf("[0,%d,com.example.foo,1,Native crash,Segmentation fault,unknown,0]", pid))
}

func (r *factRig) dropboxNative(sec int64, pid int) {
	ts := time.Unix(0, r.ns(sec)).UTC().Format("2006-01-02 15:04:05")
	r.p.ApplyDropbox("Drop box contents\n========================================\n" + ts + " data_app_native_crash (text, 100 bytes)\nProcess: com.example.foo\nPID: " +
		fmt.Sprint(pid) + "\nSignal: 11\n\n*** *** *** *** *** *** *** *** *** *** *** *** *** *** *** ***\npid: " + fmt.Sprint(pid) +
		", tid: " + fmt.Sprint(pid) + ", name: example.foo  >>> com.example.foo <<<\nsignal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000\nbacktrace:\n      #00 pc 00000000000007fc  /data/app/x/lib/x86_64/liblogtest.so (f+12)\n\n")
}

func nativeFacts(r *factRig) []*analysis.Crash {
	var out []*analysis.Crash
	for _, f := range r.facts() {
		if f.Kind == "native" {
			out = append(out, f)
		}
	}
	return out
}

// #233: the libc line is emitted when the detector lets go of it (quiet
// time passed before the tombstone lines were read), then the tombstone,
// am_crash and DropBox arrive: one fact, no second row.
func TestNativeCrashSourcesBindToTheFirstRow(t *testing.T) {
	t.Parallel()
	for _, restart := range []int{0, 1, 2, 3} { // where the service process restarts (bit0: after libc let go, bit1: after the tombstone)
		t.Run(fmt.Sprintf("restart%02b", restart), func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 4321, "com.example.foo"), r.libc(100, 4321))
			r.p.Tick(r.ns(100) + 10e9) // the detector lets go of the libc line: first row
			if n := len(nativeFacts(r)); n != 1 {
				t.Fatalf("setup: %d native facts after libc, want 1", n)
			}
			first := nativeFacts(r)[0]
			if restart&1 != 0 {
				r.restart()
			}
			r.feed(r.tombLines(102, 4321)...)
			r.p.Tick(r.ns(102) + 10e9)
			if restart&2 != 0 {
				r.restart()
			}
			r.feed(r.amCrashNative(114, 4321))
			r.p.Tick(r.ns(114) + 40e9)
			r.dropboxNative(115, 4321)
			fs := nativeFacts(r)
			if len(fs) != 1 {
				for _, f := range fs {
					t.Logf("%s pid=%d sources=%v", f.ID, f.PID, f.Sources)
				}
				t.Fatalf("%d native rows, want 1", len(fs))
			}
			f := fs[0]
			if f.ID != first.ID {
				t.Errorf("row id changed %s -> %s", first.ID, f.ID)
			}
			if !hasAll(f, "logcat:crash", "logcat:tombstone", "events:am_crash", "dropbox:data_app_native_crash") {
				t.Errorf("sources %v", f.Sources)
			}
			if f.Seq != first.Seq || f.TimeNs != first.TimeNs {
				t.Errorf("anchor moved: seq %d -> %d", first.Seq, f.Seq)
			}
			if !strings.Contains(f.Headline, "liblogtest.so") {
				t.Errorf("headline %q lacks the tombstone's location", f.Headline)
			}
		})
	}
}

// Two native crashes of one pid in different lifetimes stay two rows.
func TestNativeCrashesOfTwoLifetimesStayTwo(t *testing.T) {
	t.Parallel()
	r := newFactRig(t)
	r.feed(r.procStart(10, 4321, "com.example.foo"), r.libc(100, 4321))
	r.p.Tick(r.ns(100) + 10e9)
	r.feed(r.procStart(200, 4321, "com.example.foo"), r.libc(300, 4321))
	r.p.Tick(r.ns(300) + 10e9)
	if n := len(nativeFacts(r)); n != 2 {
		t.Fatalf("%d native rows, want 2", n)
	}
	r.feed(r.tombLines(302, 4321)...)
	r.p.Tick(r.ns(302) + 10e9)
	fs := nativeFacts(r)
	if len(fs) != 2 || hasSource(fs[0], "logcat:tombstone") || !hasSource(fs[1], "logcat:tombstone") {
		t.Fatalf("tombstone must bind to the second crash: %d rows", len(fs))
	}
}

// Race (run with -race -count=20): the ANRs of ONE process lifetime from the
// detector path while DropBox entries attach and the index is rebuilt: the
// ordinals are 1..n without a gap or a repeat, no ANR gets two ids, every
// newest line holds all three sources.
func TestOrdinalsAndPairingUnderConcurrency(t *testing.T) {
	r := newFactRig(t)
	const n = 16
	snap := func(i int, block bool) *analysis.Crash {
		c := &analysis.Crash{Kind: "anr", Boot: r.boot, TimeNs: r.ns(1000 * int64(i)), Seq: uint64(10*i + 1), PID: 7000, Process: "com.foo", Package: "com.foo", Headline: reasonA}
		if block {
			c.Seq++
			c.TimeNs += 30e9
			c.Body = "ANR in com.foo"
			c.AddEvidence(analysis.Evidence{S: "logcat:system", Q: c.Seq, T: c.TimeNs, CK: analysis.ContentKey("logcat", "com.foo", reasonA)})
		} else {
			c.AddEvidence(analysis.Evidence{S: "events:am_anr", Q: c.Seq, T: c.TimeNs, CK: analysis.ContentKey("events", "com.foo", reasonA)})
		}
		return c
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			r.p.EmitCrash(snap(i, false))
			r.p.EmitCrash(snap(i, true))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			r.dropboxANR(1000*int64(i)+40, 7000, "com.foo", reasonA)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			CompactAux(r.st, time.Now())
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	fs := anrFacts(r)
	if len(fs) != n {
		t.Fatalf("%d ANRs, want %d", len(fs), n)
	}
	for i, f := range fs {
		if f.N != i+1 {
			t.Errorf("%s: n=%d, want %d", f.ID, f.N, i+1)
		}
		if !hasAll(f, "events:am_anr", "logcat:system") {
			t.Errorf("%s: sources %v", f.ID, f.Sources)
		}
	}
}
