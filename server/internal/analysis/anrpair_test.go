package analysis

// Issue #111: the two sources of one ANR (am_anr event, "ANR in" block) and
// the DropBox entry are paired one-to-one within anrPairWindow (120 s).
// Tests go through Feed and MatchDropbox only.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func (r *jumpRig) amANR(sec float64, pid int) {
	r.feed(sec, 1500, "am_anr", 'I', fmt.Sprintf("[0,%d,com.foo,0,Input dispatching timed out]", pid))
}

func (r *jumpRig) anrBlock(sec float64, pid int) {
	r.feed(sec, 1500, "ActivityManager", model.Error, fmt.Sprintf("ANR in com.foo (com.foo/.Main)\nPID: %d\nReason: Input dispatching timed out\nCPU usage from 0ms to 1000ms:\n  10%% 1/x: 5%% user", pid))
}

func anrs(r *jumpRig) []*Crash {
	var out []*Crash
	for _, c := range r.seen {
		if c.Kind == "anr" {
			out = append(out, c)
		}
	}
	return out
}

func TestANRPairGaps(t *testing.T) {
	for _, tc := range []struct {
		gap  float64
		want int
	}{{5, 1}, {33, 1}, {100, 1}, {130, 2}} {
		r := newJumpRig(t)
		r.amANR(1000, 4321)
		r.anrBlock(1000+tc.gap, 4321)
		got := anrs(r)
		if len(got) != tc.want {
			t.Fatalf("gap %v s: %d ANRs, want %d (%s)", tc.gap, len(got), tc.want, r.kinds())
		}
		if tc.want == 1 {
			c := got[0]
			if !c.hasSource("events:am_anr") || !c.hasSource("logcat:crash") || c.Headline == "" || c.CPU == "" || c.Body == "" {
				t.Fatalf("gap %v s: not merged: sources %v headline %q cpu %q", tc.gap, c.Sources, c.Headline, c.CPU)
			}
		}
	}
}

func TestANRPairOneToOne(t *testing.T) {
	// two ANRs of one pid 40 s apart, each with its own event and block
	r := newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1033, 4321)
	r.amANR(1040, 4321)
	r.anrBlock(1073, 4321)
	if n := len(anrs(r)); n != 2 {
		t.Fatalf("two ANRs 40 s apart: %d, want 2", n)
	}
	// event, block, second event 20 s later: the first is complete
	r = newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1005, 4321)
	r.amANR(1020, 4321)
	if n := len(anrs(r)); n != 2 {
		t.Fatalf("complete ANR joined by a second am_anr: %d, want 2", n)
	}
	// block, second block: same rule for the block
	r = newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1005, 4321)
	r.anrBlock(1020, 4321)
	if n := len(anrs(r)); n != 2 {
		t.Fatalf("complete ANR joined by a second block: %d, want 2", n)
	}
}

func TestANRPairBlockFirst(t *testing.T) {
	r := newJumpRig(t)
	r.anrBlock(1000, 4321)
	r.amANR(1040, 4321)
	got := anrs(r)
	if len(got) != 1 || !got[0].hasSource("events:am_anr") {
		t.Fatalf("block then am_anr 40 s later: %d ANRs, %s", len(got), r.kinds())
	}
}

func TestANRDropboxWindow(t *testing.T) {
	r := newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1070, 4321)
	got := anrs(r)
	if len(got) != 1 {
		t.Fatalf("%d ANRs", len(got))
	}
	e := DropboxEntry{Tag: "data_app_anr", TimeNs: got[0].TimeNs + 70e9,
		Body: "Process: com.foo\nPID: 4321\nSubject: Input dispatching timed out\n\n----- pid 4321 at x -----\nCmd line: com.foo\n"}
	c, isNew := MatchDropbox(got, e)
	if isNew || c == nil || !strings.Contains(strings.Join(c.Sources, ","), "dropbox:data_app_anr") {
		t.Fatalf("dropbox entry 70 s after am_anr: new=%v c=%v", isNew, c)
	}
	e.TimeNs = got[0].TimeNs + 130e9
	if _, isNew := MatchDropbox(got, e); !isNew {
		t.Fatalf("dropbox entry 130 s after am_anr must be new")
	}
}

// A DropBox ANR dump that arrives 59, 65 or 100 s after am_anr is one ANR
// in both orders (DropBox entry first, then logcat: AdoptDropbox; logcat
// first, then DropBox entry: MatchDropbox); other kinds keep 60 s.
func TestANRDropboxWindowBothOrders(t *testing.T) {
	body := "Process: com.foo\nPID: 4321\nSubject: Input dispatching timed out\n\n----- pid 4321 at x -----\nCmd line: com.foo\n"
	for _, gap := range []int64{59, 65, 100} {
		dbOnly := &Crash{ID: "c1", Kind: "anr", TimeNs: 1000e9 + gap*1e9, PID: 4321, Process: "com.foo", Sources: []string{"dropbox:data_app_anr"}}
		fresh := &Crash{Kind: "anr", TimeNs: 1000e9, PID: 4321, Process: "com.foo", Sources: []string{"events:am_anr"}}
		if !AdoptDropbox([]*Crash{dbOnly}, fresh) || fresh.ID != "c1" {
			t.Errorf("AdoptDropbox: dropbox %d s after am_anr not adopted", gap)
		}
		logc := &Crash{ID: "c2", Kind: "anr", TimeNs: 1000e9, PID: 4321, Process: "com.foo", Sources: []string{"events:am_anr"}}
		e := DropboxEntry{Tag: "data_app_anr", TimeNs: 1000e9 + gap*1e9, Body: body}
		if c, isNew := MatchDropbox([]*Crash{logc}, e); isNew || c == nil || c.ID != "c2" {
			t.Errorf("MatchDropbox: dropbox %d s after am_anr gave a new ANR", gap)
		}
	}
	// other kinds keep the 60 s window
	for _, tc := range []struct {
		gap  int64
		want bool
	}{{59, true}, {65, false}} {
		dbOnly := &Crash{ID: "j1", Kind: "java", TimeNs: 1000e9 + tc.gap*1e9, PID: 4321, Process: "com.foo", Sources: []string{"dropbox:data_app_crash"}}
		fresh := &Crash{Kind: "java", TimeNs: 1000e9, PID: 4321, Process: "com.foo"}
		if got := AdoptDropbox([]*Crash{dbOnly}, fresh); got != tc.want {
			t.Errorf("java crash, dropbox %d s apart: adopted=%v, want %v", tc.gap, got, tc.want)
		}
	}
}
