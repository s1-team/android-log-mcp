package mcpserver

// Issue #55: the crash list is ordered by arrival, not by device time, and
// the detector follows device clock jumps found by the pipeline.

import (
	"fmt"
	"strings"
	"testing"
)

func jumpCrashLines(sec float64, pid int, msg string) []string {
	return []string{
		fxLine(sec, 10200, pid, "E", "AndroidRuntime", "FATAL EXCEPTION: main"),
		fxLine(sec, 10200, pid, "E", "AndroidRuntime", fmt.Sprintf("Process: com.foo, PID: %d", pid)),
		fxLine(sec, 10200, pid, "E", "AndroidRuntime", "java.lang.IllegalStateException: "+msg),
		fxLine(sec, 10200, pid, "E", "AndroidRuntime", "\tat com.foo."+msg+".run(Main.java:1)"),
	}
}

func TestCrashListFollowsArrivalAcrossClockJumps(t *testing.T) {
	e := newReplay(t, fxBoot1)
	const t0 = 1791572000.0
	var lines []string
	filler := func(sec float64) {
		lines = append(lines, fxLine(sec, 1000, 1500, "I", "Filler", "line"))
	}
	filler(t0)
	lines = append(lines, jumpCrashLines(t0+1, 100, "crashA")...)
	filler(t0 + 30)
	lines = append(lines, jumpCrashLines(t0+60, 101, "crashB")...)
	filler(t0 + 62)
	// the clock is set back two hours: later crashes have older device times
	filler(t0 - 7200 + 100)
	lines = append(lines, jumpCrashLines(t0-7200+110, 102, "crashC")...)
	filler(t0 - 7200 + 130)
	// and forward by a year
	filler(t0 + 365*86400)
	lines = append(lines, jumpCrashLines(t0+365*86400+10, 103, "crashD")...)
	filler(t0 + 365*86400 + 20)
	e.replay(strings.Join(lines, "\n"))

	out := e.ok("logcat_crashes", `{"limit":10}`, "crashA", "crashB", "crashC", "crashD")
	dump(t, out)
	pos := func(s string) int { return strings.Index(out, s) }
	if !(pos("crashD") < pos("crashC") && pos("crashC") < pos("crashB") && pos("crashB") < pos("crashA")) {
		t.Errorf("crash list must be newest arrival first (D, C, B, A):\n%s", out)
	}
	// ids stay: cr1..cr4 in arrival order
	for i, m := range []string{"crashA", "crashB", "crashC", "crashD"} {
		if got := e.ok("logcat_crashes", fmt.Sprintf(`{"id":"cr%d"}`, i+1), m); !strings.Contains(got, fmt.Sprintf("cr%d", i+1)) {
			t.Errorf("cr%d: %s", i+1, got)
		}
	}
}

func TestClockJumpInsideACrashDoesNotSplitIt(t *testing.T) {
	e := newReplay(t, fxBoot1)
	const t0 = 1791572000.0
	// am_crash, then the clock is set back 2 h, then the stack of the same
	// crash: one crash with both sources
	lines := []string{
		fxLine(t0, 1000, 1500, "I", "Filler", "line"),
		fxLine(t0+1, 1000, 1500, "I", "am_crash", "[100,0,com.foo,1,java.lang.IllegalStateException,split,Main.java,1]"),
		fxLine(t0-7200+2, 1000, 1500, "I", "Filler", "after the clock was set back"),
	}
	lines = append(lines, jumpCrashLines(t0-7200+3, 100, "split")...)
	lines = append(lines, fxLine(t0-7200+30, 1000, 1500, "I", "Filler", "later"))
	e.replay(strings.Join(lines, "\n"))
	out := e.ok("logcat_crashes", `{}`, "ok view=detail", "split", "events:am_crash")
	dump(t, out)
}
