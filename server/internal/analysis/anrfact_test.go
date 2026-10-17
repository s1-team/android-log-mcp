package analysis

import "testing"

// Issue #225 PR2: ANR pairing by ordinal and interval; the cap, the reason
// compatibility and the legacy windows, through FindOpenFact (the stored
// facts) and Feed (detector memory).

func TestReasonCompatible(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b      string
		ok, known bool
	}{
		{"Input dispatching timed out", "Input dispatching timed out", true, true},
		{"Input dispatching timed out", "Input dispatching timed out (Waited 5001ms)", true, true},
		{"Broadcast of Intent { act=A cmp=x }", "Broadcast of Intent { act=A cmp=x } (extra)", true, true},
		{"executing service com.foo/.S", "executing service com.foo/.S {x}", true, true},
		{"Input dispatching timed out (Waited 5001ms)", "Input dispatching timed out (Waited 9ms for X)", true, true},
		{"Input dispatching timed out", "Broadcast of Intent { act=A }", false, true},
		{"", "Broadcast of Intent { act=A }", true, false},
		{"x", "", true, false},
	} {
		if ok, known := ReasonCompatible(tc.a, tc.b); ok != tc.ok || known != tc.known {
			t.Errorf("%q vs %q: ok=%v known=%v, want %v %v", tc.a, tc.b, ok, known, tc.ok, tc.known)
		}
	}
}

func TestFindOpenFactCapBoundary(t *testing.T) {
	t.Parallel()
	f := anrFact("anr1", 1000, 7, evAnr(1000))
	cap := anrEvidenceCap / 1e9
	for _, tc := range []struct {
		gap  int64
		want bool
	}{{cap - 1, true}, {cap, true}, {cap + 1, false}} {
		in := anrFact("", 1000+tc.gap, 7, evBlock(uint64(1000+tc.gap)))
		in.Headline = f.Headline
		got, _ := FindOpenFact([]*Crash{f}, in, nil, nil)
		if (got != nil) != tc.want {
			t.Errorf("cap%+d s: attached=%v, want %v", tc.gap-cap, got != nil, tc.want)
		}
	}
	// the cap is on the jump-normalized axis: a stored back-jump between the
	// anchor and the block does not widen or shrink it
	jumps := []ClockShift{{Seq: 1500, DeltaNs: -86400e9}}
	in := anrFact("", 1000+cap-86400, 7, evBlock(2000))
	in.Headline = f.Headline
	if got, _ := FindOpenFact([]*Crash{f}, in, jumps, nil); got != f {
		t.Error("block after a back-jump, cap-1 s on the normalized axis, not attached")
	}
}

func TestFindOpenFactIntervalRule(t *testing.T) {
	t.Parallel()
	f1 := anrFact("anr1", 100, 7, evAnr(100))
	f2 := anrFact("anr2", 200, 7, evAnr(200))
	in := func(sec int64, e Evidence) *Crash { c := anrFact("", sec, 7, e); c.Boot = "b1"; return c }
	// block anchored before F2: the latest fact anchored before it takes it
	if got, _ := FindOpenFact([]*Crash{f2, f1}, in(230, evBlock(230)), nil, nil); got != f2 {
		t.Errorf("block at 230: %v", got)
	}
	// block between the two: only F1 is anchored before it
	if got, _ := FindOpenFact([]*Crash{f2, f1}, in(150, evBlock(150)), nil, nil); got != f1 {
		t.Errorf("block at 150: %v", got)
	}
	// F2 has its block: a later block must not fall back to F1 (open, but
	// not the latest of the lifetime): a new fact
	f2b := anrFact("anr2", 200, 7, evAnr(200), evBlock(230))
	if got, _ := FindOpenFact([]*Crash{f2b, f1}, in(400, evBlock(400)), nil, nil); got != nil {
		t.Errorf("block at 400 attached to %s", got.ID)
	}
	// anchored after the evidence by more than the slack: never
	if got, _ := FindOpenFact([]*Crash{f2}, in(190, evBlock(0)), nil, nil); got != nil {
		t.Errorf("evidence before the anchor attached to %s", got.ID)
	}
}

func TestFindOpenFactReasons(t *testing.T) {
	t.Parallel()
	f := anrFact("anr1", 100, 7, evAnr(100))
	f.Headline = "Input dispatching timed out"
	in := func(h string) *Crash { c := anrFact("", 130, 7, evBlock(130)); c.Boot = "b1"; c.Headline = h; return c }
	if got, _ := FindOpenFact([]*Crash{f}, in("Broadcast of Intent { act=A }"), nil, nil); got != nil {
		t.Error("a different reason attached")
	}
	if got, _ := FindOpenFact([]*Crash{f}, in("Input dispatching timed out (Waited 5001ms)"), nil, nil); got != f {
		t.Error("compatible reason did not attach")
	}
	// missing reason: one open candidate attaches, two do not
	if got, _ := FindOpenFact([]*Crash{f}, in(""), nil, nil); got != f {
		t.Error("missing reason with one candidate did not attach")
	}
	g := anrFact("anr2", 110, 7, evAnr(110))
	g.Headline = "Input dispatching timed out"
	if got, _ := FindOpenFact([]*Crash{g, f}, in(""), nil, nil); got != nil {
		t.Errorf("missing reason with two candidates attached to %s", got.ID)
	}
}

// A fact without ev (0.9.x) keeps the 120 s window, and is not paired by the
// interval rule.
func TestFindOpenFactLegacyWindow(t *testing.T) {
	t.Parallel()
	l := &Crash{ID: "anr1", Kind: "anr", Boot: "b1", TimeNs: 1000e9, Seq: 1000, PID: 7, Process: "com.foo", Headline: "r", Sources: []string{"events:am_anr"}}
	for _, tc := range []struct {
		gap  int64
		want bool
	}{{120, true}, {121, false}} {
		in := anrFact("", 1000+tc.gap, 7, evBlock(uint64(1000+tc.gap)))
		if got, _ := FindOpenFact([]*Crash{l}, in, nil, nil); (got != nil) != tc.want {
			t.Errorf("legacy +%d s: %v", tc.gap, got)
		}
	}
}

// Detector memory: the same rules without a store (Feed only).
func TestDetectorPairsByInterval(t *testing.T) {
	t.Parallel()
	r := newJumpRig(t)
	r.amANR(1000, 4321)
	r.amANR(1200, 4321) // the block of the first ANR was lost
	r.anrBlock(1230, 4321)
	got := anrs(r)
	if len(got) != 2 || got[0].Body != "" || got[1].Body == "" {
		t.Fatalf("%d ANRs; block must go to the second: %s", len(got), r.kinds())
	}
	// a complete ANR does not take a later block from the lifetime: new ANR
	r.anrBlock(1500, 4321)
	if n := len(anrs(r)); n != 3 {
		t.Fatalf("%d ANRs, want 3", n)
	}
}
