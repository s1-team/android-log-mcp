package analysis

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #175: since the one-to-one pairing (#111) a record fed again found no
// ANR lacking its source and opened a second, partial ANR. The same record
// (same pid, same seq) is now an idempotent repeat. Repeats of the same text
// under another seq are not covered by this rule.

// feedAt feeds a record with an explicit seq (the rig numbers its own).
func (r *jumpRig) feedAt(seq uint64, sec float64, tag string, lvl model.Level, msg string) {
	r.d.Feed(&model.Record{TimeNs: int64(sec * 1e9), PID: 1500, TID: 1500, Tag: tag, Level: lvl, Msg: msg, Seq: seq, Boot: "b1", Buffer: "crash"})
}

func (r *jumpRig) eventAt(seq uint64, sec float64, pid int) {
	r.feedAt(seq, sec, "am_anr", 'I', fmt.Sprintf("[0,%d,com.foo,0,Input dispatching timed out]", pid))
}

func (r *jumpRig) blockAt(seq uint64, sec float64, pid int) {
	r.feedAt(seq, sec, "ActivityManager", model.Error, fmt.Sprintf("ANR in com.foo (com.foo/.Main)\nPID: %d\nReason: Input dispatching timed out\nCPU usage from 0ms to 1000ms:\n  10%% 1/x: 5%% user", pid))
}

func TestANRSameSeqRepeatIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		f    func(r *jumpRig)
	}{
		{"event, block, block again", func(r *jumpRig) { r.eventAt(1, 1000, 4321); r.blockAt(2, 1005, 4321); r.blockAt(2, 1005, 4321) }},
		{"event, block, event again", func(r *jumpRig) { r.eventAt(1, 1000, 4321); r.blockAt(2, 1005, 4321); r.eventAt(1, 1000, 4321) }},
		{"event, event again (no block)", func(r *jumpRig) { r.eventAt(1, 1000, 4321); r.eventAt(1, 1000, 4321) }},
		{"block, block again (no event)", func(r *jumpRig) { r.blockAt(2, 1005, 4321); r.blockAt(2, 1005, 4321) }},
		{"block, block again 30 s later by arrival", func(r *jumpRig) { r.blockAt(2, 1005, 4321); r.blockAt(2, 1035, 4321) }},
	} {
		r := newJumpRig(t)
		tc.f(r)
		if got := anrs(r); len(got) != 1 {
			t.Errorf("%s: %d ANRs, want 1", tc.name, len(got))
		}
	}
}

// The repeat changes nothing: the ANR is the same value as without it.
func TestANRSameSeqRepeatLeavesTheANRUnchanged(t *testing.T) {
	t.Parallel()
	once, twice := newJumpRig(t), newJumpRig(t)
	once.eventAt(1, 1000, 4321)
	once.blockAt(2, 1005, 4321)
	twice.eventAt(1, 1000, 4321)
	twice.blockAt(2, 1005, 4321)
	twice.eventAt(1, 1000, 4321)
	twice.blockAt(2, 1005, 4321)
	a, b := anrs(once), anrs(twice)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("%d and %d ANRs", len(a), len(b))
	}
	if !reflect.DeepEqual(*a[0], *b[0]) {
		t.Errorf("a repeat changed the ANR:\n%+v\n%+v", *a[0], *b[0])
	}
}

// A genuinely different ANR of the same process stays separate, with or
// without a repeat in between.
func TestANRSecondGenuineANRStaysSeparate(t *testing.T) {
	t.Parallel()
	r := newJumpRig(t)
	r.eventAt(1, 1000, 4321)
	r.blockAt(2, 1005, 4321)
	r.blockAt(2, 1005, 4321) // repeat
	r.eventAt(3, 1060, 4321) // 60 s later: a second ANR
	r.blockAt(4, 1065, 4321)
	r.blockAt(4, 1065, 4321) // repeat
	got := anrs(r)
	if len(got) != 2 {
		t.Fatalf("%d ANRs, want 2", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 3 || got[0].Body == "" || got[1].Body == "" {
		t.Errorf("seqs %d and %d, bodies %q / %q", got[0].Seq, got[1].Seq, got[0].Body, got[1].Body)
	}
	// another pid's ANR with the same seq numbers is not a repeat either
	r2 := newJumpRig(t)
	r2.eventAt(1, 1000, 4321)
	r2.eventAt(1, 1000, 5555)
	if len(anrs(r2)) != 2 {
		t.Errorf("two pids merged: %d ANRs", len(anrs(r2)))
	}
}
