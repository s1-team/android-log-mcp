package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func seqRec(sec int64, pid int, tag, msg string) *model.Record {
	return &model.Record{TimeNs: 1_700_000_000e9 + sec*1e9, PID: pid, TID: pid, UID: 10001, Level: 'I', Tag: tag, Msg: msg}
}

// Issue #55: after a back jump a pid reused by another app is attributed by
// arrival order, not by device time (no emulator, no adb).
func TestPidReuseAcrossBackJumpAttributedBySeq(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	p.Procs.SetPackages(map[int][]string{10001: {"com.foo"}, 10002: {"com.bar"}})
	now := time.Now()
	in := []*model.Record{
		seqRec(1000, 1880, "am_proc_start", "[0,50,10001,com.foo,service,{com.foo/com.foo.S}]"),
		seqRec(1005, 50, "app", "foo before jump"),
		seqRec(1010, 1880, "am_proc_died", "[0,50,com.foo,900,19]"),
		seqRec(400, 1880, "am_proc_start", "[0,50,10002,com.bar,service,{com.bar/com.bar.S}]"), // clock set back
		seqRec(410, 50, "app", "bar 410"),
		seqRec(1005, 50, "app", "bar 1005"), // same device time as a foo line
		seqRec(1600, 50, "app", "bar 1600"),
	}
	for _, r := range in {
		p.Accept(r, now)
	}
	if err := p.Drain(true, now); err != nil {
		t.Fatal(err)
	}
	if n := p.SeqMismatches(); n != 0 {
		t.Fatalf("seq mismatches: %d", n)
	}
	got := map[string]string{}
	st.Scan(0, st.LastSeq()+1, true, func(r *model.Record) bool {
		got[r.Msg] = r.Process
		return true
	})
	for msg, want := range map[string]string{
		"foo before jump": "com.foo",
		"bar 410":         "com.bar",
		"bar 1005":        "com.bar",
		"bar 1600":        "com.bar",
	} {
		if got[msg] != want {
			t.Errorf("%q attributed to %q, want %q", msg, got[msg], want)
		}
	}
	if lt := p.Procs.LookupSeq(50, 1005e9, 2); lt == nil || lt.Name != "com.foo" || lt.StartSeq != 1 || lt.EndSeq != 3 {
		t.Fatalf("com.foo lifetime seqs: %+v", lt)
	}
}

// A prediction that does not match the store's seq is counted and falls
// back to the time rule; the record still stores under the real seq.
func TestSeqPredictionMismatchFallsBackToTime(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	now := time.Now()
	p.Accept(seqRec(10, 1880, "am_proc_start", "[0,60,10001,com.foo,service,{com.foo/com.foo.S}]"), now)
	line := seqRec(11, 60, "app", "hello")
	p.Accept(line, now)
	line.Seq += 7 // forced mismatch
	if err := p.Drain(true, now); err != nil {
		t.Fatal(err)
	}
	if n := p.SeqMismatches(); n != 1 {
		t.Fatalf("mismatches %d, want 1", n)
	}
	got := map[string]bool{}
	st.Scan(0, st.LastSeq()+1, true, func(r *model.Record) bool {
		got[fmt.Sprintf("%d:%s", r.Seq, r.Process)] = true
		return true
	})
	if len(got) != 2 || !got["2:com.foo"] {
		t.Fatalf("stored %v", got)
	}
}
