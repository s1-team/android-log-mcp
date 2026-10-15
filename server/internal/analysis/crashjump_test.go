package analysis

// Issue #55: the detector's windows and expiry follow the device clock
// through jumps (Detector.ClockJump), so a jump neither splits nor merges
// a crash and a pending crash does not wait for the clock to catch up.
// Tests go through Feed / ClockJump / Tick and Emit only.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

type jumpRig struct {
	t    *testing.T
	d    *Detector
	seen []*Crash // distinct crashes, in emit order
	host time.Time
	seq  uint64
}

func newJumpRig(t *testing.T) *jumpRig {
	r := &jumpRig{t: t, d: NewDetector(), host: time.Unix(1_800_000_000, 0)}
	r.d.Now = func() time.Time { return r.host }
	r.d.Emit = func(c *Crash) {
		for _, s := range r.seen {
			if s == c {
				return
			}
		}
		r.seen = append(r.seen, c)
	}
	return r
}

// feed sends one line at device time sec; the host clock does not move.
func (r *jumpRig) feed(sec float64, pid int, tag string, lvl model.Level, msg string) {
	r.seq++
	r.d.Feed(&model.Record{TimeNs: int64(sec * 1e9), PID: pid, TID: pid, Tag: tag, Level: lvl, Msg: msg, Seq: r.seq, Boot: "b1", Buffer: "crash"})
}

func (r *jumpRig) amCrash(sec float64, pid int, proc, msg string) {
	r.feed(sec, 1500, "am_crash", 'I', fmt.Sprintf("[%d,0,%s,1,java.lang.IllegalStateException,%s,Foo.java,10]", pid, proc, msg))
}

func (r *jumpRig) fatal(sec float64, pid int, proc, msg string) {
	r.feed(sec, pid, "AndroidRuntime", model.Error, fmt.Sprintf("FATAL EXCEPTION: main\nProcess: %s, PID: %d\njava.lang.IllegalStateException: %s\n\tat %s.Main.run(Main.java:1)", proc, pid, msg, proc))
}

func (r *jumpRig) libc(sec float64, pid int, proc string) {
	r.feed(sec, pid, "libc", model.Fatal, fmt.Sprintf("Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid %d (RenderThread), pid %d (%s)", pid, pid, proc))
}

func (r *jumpRig) tomb(sec float64, pid int, proc string) {
	dpid := 6000
	for i, m := range []string{
		"*** *** *** *** *** *** *** *** *** *** *** *** *** *** *** ***",
		fmt.Sprintf("pid: %d, tid: %d, name: RenderThread  >>> %s <<<", pid, pid, proc),
		"signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000",
		"backtrace:",
		"      #00 pc 000000000001a2b4  /data/app/x/lib/arm64/libfoo.so (f+44)",
	} {
		r.feed(sec+float64(i)*0.0001, dpid, "DEBUG", model.Fatal, m)
	}
}

// tick advances the host clock and ticks with 0 (no device time).
func (r *jumpRig) tick(d time.Duration) {
	r.host = r.host.Add(d)
	r.d.Tick(0)
}

func (r *jumpRig) kinds() string {
	var s []string
	for _, c := range r.seen {
		s = append(s, fmt.Sprintf("%s:%d:%s", c.Kind, c.PID, strings.Join(c.Sources, "+")))
	}
	return strings.Join(s, " | ")
}

func TestDetectorJumpDoesNotSplitJava(t *testing.T) {
	// am_crash, the clock is set back 100 s, then the stack of the same crash
	for _, jump := range []bool{false, true} {
		r := newJumpRig(t)
		r.amCrash(10000, 4321, "com.foo", "boom")
		if jump {
			r.d.ClockJump(-100e9)
			r.fatal(9900.5, 4321, "com.foo", "boom")
		} else {
			r.fatal(10000.5, 4321, "com.foo", "boom")
		}
		if len(r.seen) != 1 || len(r.seen[0].Sources) != 2 {
			t.Errorf("jump=%v: want one crash with both sources, got %s", jump, r.kinds())
		}
		if jump && r.seen[0].TimeNs != int64(9900.5e9) {
			t.Errorf("Crash.TimeNs must stay device time, got %d", r.seen[0].TimeNs)
		}
	}
}

func TestDetectorJumpDoesNotMergeCrashes(t *testing.T) {
	// two crashes of one pid number, 5 s apart in device time, with a clock
	// set back between them (pid reuse, 3000 s apart really): two crashes
	r := newJumpRig(t)
	r.fatal(10000, 100, "com.foo", "first")
	r.d.ClockJump(-3000e9)
	r.fatal(10005, 100, "com.foo", "second") // 3000 s later on the old clock
	if len(r.seen) != 2 || r.seen[0].Headline == r.seen[1].Headline {
		t.Fatalf("want two crashes, got %s", r.kinds())
	}
	// control: without a jump the same pair is one crash (15 s window)
	r = newJumpRig(t)
	r.fatal(10000, 100, "com.foo", "first")
	r.fatal(10005, 100, "com.foo", "second")
	if len(r.seen) != 1 {
		t.Fatalf("control: want one crash, got %s", r.kinds())
	}
}

func TestDetectorForwardJumpKeepsNativeTogether(t *testing.T) {
	r := newJumpRig(t)
	r.libc(1000, 5000, "com.foo")
	r.d.ClockJump(4000e9)
	r.tomb(5000.3, 5000, "com.foo")
	r.tick(10 * time.Second)
	if len(r.seen) != 1 || !strings.Contains(r.kinds(), "logcat:tombstone") {
		t.Fatalf("want one native crash with the tombstone, got %s", r.kinds())
	}
}

func TestDetectorBackJumpThenQuietEnds(t *testing.T) {
	r := newJumpRig(t)
	r.libc(1000, 5000, "com.foo")
	r.d.ClockJump(-500e9)
	r.feed(500.5, 1500, "ActivityManager", 'I', "unrelated")
	if len(r.seen) != 0 {
		t.Fatalf("pending native crash emitted too early: %s", r.kinds())
	}
	r.tick(2 * time.Second)
	if len(r.seen) != 0 {
		t.Fatalf("emitted before the quiet period: %s", r.kinds())
	}
	r.tick(6 * time.Second) // 5 s of the new clock passed, nothing arrived
	if len(r.seen) != 1 {
		t.Fatalf("a quiet pending native crash must end: %s", r.kinds())
	}
}

func TestDetectorTickZeroEndsQuietNativeWithoutJump(t *testing.T) {
	r := newJumpRig(t)
	r.libc(1000, 5000, "com.foo")
	r.tick(time.Second)
	if len(r.seen) != 0 {
		t.Fatal("ended after 1 s")
	}
	r.tick(10 * time.Second)
	if len(r.seen) != 1 {
		t.Fatalf("Tick(0) must end a quiet pending native crash: %s", r.kinds())
	}
}

func TestDetectorPossibleJumpShiftsNothing(t *testing.T) {
	// a quiet hour is not a clock set: the pipeline does not call ClockJump
	// and the pending crash expires on the next line, as before
	r := newJumpRig(t)
	r.libc(1000, 5000, "com.foo")
	r.feed(1000+7200, 1500, "ActivityManager", 'I', "after a quiet two hours")
	if len(r.seen) != 1 {
		t.Fatalf("want the crash emitted: %s", r.kinds())
	}
}

func TestDetectorJumpANRAndAmNativeWindows(t *testing.T) {
	r := newJumpRig(t)
	r.feed(10000, 1500, "am_anr", 'I', "[0,4321,com.foo,0,Input dispatching timed out]")
	r.d.ClockJump(-5000e9)
	r.feed(5010, 1500, "ActivityManager", model.Error, "ANR in com.foo (com.foo/.Main)\nPID: 4321\nReason: Input dispatching timed out")
	if len(r.seen) != 1 || r.seen[0].Kind != "anr" {
		t.Fatalf("30 s ANR window across a jump: %s", r.kinds())
	}
	// am_crash "Native crash" waits for the libc lines up to 60 s
	r = newJumpRig(t)
	r.feed(20000, 1500, "am_crash", 'I', "[1500,0,com.foo,1,Native crash,Segmentation fault,,0]")
	r.d.ClockJump(-9000e9)
	r.libc(11020, 5000, "com.foo")
	r.tick(10 * time.Second)
	if len(r.seen) != 1 || !strings.Contains(r.kinds(), "events:am_crash") {
		t.Fatalf("60 s amNative window across a jump: %s", r.kinds())
	}
}

// Property: inserting clock jumps into a timeline (device times shifted by
// the accumulated deltas, a ClockJump call at each) does not change which
// crashes are found, compared with the same timeline without jumps (the
// brute-force oracle).
func TestDetectorJumpsPropertyAgainstNoJumpOracle(t *testing.T) {
	type ev struct {
		dt   float64 // seconds after the previous event
		kind int
		pid  int
	}
	for seed := int64(0); seed < 200; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		var evs []ev
		for i, n := 0, 4+rnd.Intn(8); i < n; i++ {
			gap := []float64{0.2, 3, 12, 40, 200}[rnd.Intn(5)]
			evs = append(evs, ev{gap, rnd.Intn(3), 100 + rnd.Intn(3)})
		}
		run := func(jumps bool) []string {
			r := newJumpRig(t)
			rr := rand.New(rand.NewSource(seed + 1000))
			dev, shift := 100000.0, 0.0
			for i, e := range evs {
				dev += e.dt
				if jumps && i > 0 && rr.Intn(3) == 0 {
					delta := float64(rr.Intn(2000)-1000) + 0.5
					if delta > -1 && delta < 1 {
						delta = 50
					}
					r.d.ClockJump(int64(delta * 1e9))
					shift += delta
				}
				t := dev + shift
				proc := fmt.Sprintf("com.p%d", e.pid)
				switch e.kind {
				case 0:
					r.amCrash(t, e.pid, proc, "x")
				case 1:
					r.fatal(t, e.pid, proc, "x")
				default:
					r.feed(t, e.pid, "am_anr", 'I', fmt.Sprintf("[0,%d,%s,0,why]", e.pid, proc))
				}
			}
			r.d.Tick(1e18) // flush everything
			var out []string
			for _, c := range r.seen {
				out = append(out, fmt.Sprintf("%s:%d:%d", c.Kind, c.PID, len(c.Sources)))
			}
			sort.Strings(out)
			return out
		}
		want, got := run(false), run(true)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Fatalf("seed %d: with jumps %v, without %v", seed, got, want)
		}
	}
}
