package analysis

// Issue #93: DropBox matching compares the entry time with the crash time
// as stored and shifted by the clock jumps stored after the crash. Tests go
// through MatchDropboxBoot and AdoptDropbox only; the crashes are built by
// hand so every delta is exact.

import (
	"fmt"
	"testing"
)

const dbT0 = int64(1_800_000_000e9) // crash device time

func dbCrash(kind, id, boot string, pid int, proc string, t int64, seq uint64, dropboxOnly bool) *Crash {
	c := &Crash{ID: id, Kind: kind, Boot: boot, PID: pid, Process: proc, TimeNs: t, Seq: seq, Sources: []string{"logcat:crash"}}
	if dropboxOnly {
		c.Sources = []string{"dropbox:data_app_crash"}
		if kind == "anr" {
			c.Sources = []string{"dropbox:data_app_anr"}
		}
	}
	return c
}

func dbEntry(kind string, pid int, proc string, t int64) DropboxEntry {
	if kind == "anr" {
		return DropboxEntry{Tag: "data_app_anr", TimeNs: t, Body: fmt.Sprintf("Process: %s\nPID: %d\nSubject: Input dispatching timed out\n\n----- pid %d at x -----\nCmd line: %s\n", proc, pid, pid, proc)}
	}
	return DropboxEntry{Tag: "data_app_crash", TimeNs: t, Body: fmt.Sprintf("Process: %s\nPID: %d\n\njava.lang.IllegalStateException: boom\n\tat Foo.run(Foo.java:1)\n", proc, pid)}
}

func TestDropboxClockJumpMatch(t *testing.T) {
	const s = int64(1e9)
	fwd := []ClockShift{{Seq: 20, DeltaNs: 3600 * s}}
	back := []ClockShift{{Seq: 20, DeltaNs: -3600 * s}}
	for _, kind := range []string{"java", "anr"} {
		win := 60 * s
		if kind == "anr" {
			win = legacyAnrWindow
		}
		for _, tc := range []struct {
			name   string
			jumps  []ClockShift
			boot   string // boot of the entry; the crash is in b1
			pid    int
			proc   string
			entry  int64 // entry time
			cseq   uint64
			wantID bool // matched
		}{
			{"no jump, inside", nil, "b1", 100, "com.foo", dbT0 + win - 1*s, 10, true},
			{"no jump, window edge is outside", nil, "b1", 100, "com.foo", dbT0 + win, 10, false},
			{"no jump, just inside before", nil, "b1", 100, "com.foo", dbT0 - win + 1*s, 10, true},
			{"no jump, outside before", nil, "b1", 100, "com.foo", dbT0 - win, 10, false},
			{"no jump, shift-sized gap does not match", nil, "b1", 100, "com.foo", dbT0 + 3600*s, 10, false},
			{"forward jump, entry after it", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + 5*s, 10, true},
			{"forward jump, entry before it (raw)", fwd, "b1", 100, "com.foo", dbT0 + 5*s, 10, true},
			{"forward jump, shifted edge inside", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + win - 1*s, 10, true},
			{"forward jump, shifted edge outside", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + win, 10, false},
			{"forward jump, shifted edge outside before", fwd, "b1", 100, "com.foo", dbT0 + 3600*s - win, 10, false},
			{"forward jump, in between: no match", fwd, "b1", 100, "com.foo", dbT0 + 1800*s, 10, false},
			{"back jump, entry after it", back, "b1", 100, "com.foo", dbT0 - 3600*s + 30*s, 10, true},
			{"back jump, entry before it (raw)", back, "b1", 100, "com.foo", dbT0 - 30*s, 10, true},
			{"back jump, shifted edge inside", back, "b1", 100, "com.foo", dbT0 - 3600*s - win + 1*s, 10, true},
			{"back jump, shifted edge outside", back, "b1", 100, "com.foo", dbT0 - 3600*s - win, 10, false},
			{"jump before the crash is not applied", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + 5*s, 25, false},
			{"jump at the crash's own line is not applied", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + 5*s, 20, false},
			{"crash without seq is not shifted", fwd, "b1", 100, "com.foo", dbT0 + 3600*s + 5*s, 0, false},
			{"two jumps add up", []ClockShift{{Seq: 20, DeltaNs: 3600 * s}, {Seq: 30, DeltaNs: -600 * s}}, "b1", 100, "com.foo", dbT0 + 3000*s + 7*s, 10, true},
			{"jump, other boot of the entry", fwd, "b2", 100, "com.foo", dbT0 + 3600*s + 5*s, 10, false},
			{"jump, pid mismatch", fwd, "b1", 101, "com.foo", dbT0 + 3600*s + 5*s, 10, false},
			{"jump, process mismatch", fwd, "b1", 100, "com.bar", dbT0 + 3600*s + 5*s, 10, false},
			{"no jump, pid mismatch", nil, "b1", 101, "com.foo", dbT0 + 5*s, 10, false},
			{"no jump, process mismatch", nil, "b1", 100, "com.bar", dbT0 + 5*s, 10, false},
		} {
			t.Run(kind+"/match/"+tc.name, func(t *testing.T) {
				x := dbCrash(kind, "c1", "b1", 100, "com.foo", dbT0, tc.cseq, false)
				// boot "" keeps the same-boot far fallback of Java crashes
				// (issue #63) out of the window checks
				boot := tc.boot
				if boot == "b1" {
					boot = ""
				}
				c, isNew := MatchDropboxBoot([]*Crash{x}, dbEntry(kind, tc.pid, tc.proc, tc.entry), boot, tc.jumps)
				if c == nil {
					t.Fatalf("nil crash")
				}
				if got := !isNew && c.ID == "c1"; got != tc.wantID {
					t.Fatalf("matched=%v, want %v", got, tc.wantID)
				}
			})
			t.Run(kind+"/adopt/"+tc.name, func(t *testing.T) {
				// the DropBox-only crash is the entry's; the logcat crash comes later
				only := dbCrash(kind, "d1", tc.boot, tc.pid, tc.proc, tc.entry, 0, true)
				fresh := dbCrash(kind, "", "b1", 100, "com.foo", dbT0, tc.cseq, false)
				fresh.Sources = []string{"logcat:crash"}
				if tc.pid == 100 && tc.proc != "com.foo" {
					t.Skip("AdoptDropbox keys on pid, not on the process name")
				}
				if got := AdoptDropbox([]*Crash{only}, fresh, tc.jumps); got != tc.wantID {
					t.Fatalf("adopted=%v, want %v", got, tc.wantID)
				}
				if tc.wantID && fresh.ID != "d1" {
					t.Fatalf("ID %q", fresh.ID)
				}
			})
		}
	}
}

// Two different crashes of one process, one per clock epoch, equally near
// the entry (one of them through the shift): the entry stays unmatched.
func TestDropboxClockJumpAmbiguous(t *testing.T) {
	const s = int64(1e9)
	jumps := []ClockShift{{Seq: 15, DeltaNs: 3600 * s}}
	a := dbCrash("anr", "a", "b1", 100, "com.foo", dbT0, 10, false)             // before the jump
	b := dbCrash("anr", "b", "b1", 100, "com.foo", dbT0+3600*s+60*s, 20, false) // after it
	e := dbEntry("anr", 100, "com.foo", dbT0+3600*s+30*s)
	if c, isNew := MatchDropboxBoot([]*Crash{a, b}, e, "b1", jumps); !isNew || c == nil || c.ID != "" {
		t.Fatalf("equal deltas must stay unmatched: new=%v c=%+v", isNew, c)
	}
	// reverse list order gives the same answer
	if _, isNew := MatchDropboxBoot([]*Crash{b, a}, e, "b1", jumps); !isNew {
		t.Fatalf("list order changed the result")
	}
	// the nearer one wins
	e.TimeNs = dbT0 + 3600*s + 40*s // a: 40 s shifted, b: 20 s raw
	if c, isNew := MatchDropboxBoot([]*Crash{a, b}, e, "b1", jumps); isNew || c.ID != "b" {
		t.Fatalf("nearer crash b must win: new=%v c=%+v", isNew, c)
	}
	e.TimeNs = dbT0 + 3600*s + 10*s // a: 10 s shifted, b: 50 s raw
	if c, isNew := MatchDropboxBoot([]*Crash{a, b}, e, "b1", jumps); isNew || c.ID != "a" {
		t.Fatalf("nearer crash a must win: new=%v c=%+v", isNew, c)
	}
	// Adopt: two DropBox-only crashes
	da := dbCrash("anr", "da", "b1", 100, "com.foo", dbT0+3600*s+30*s, 0, true)
	db := dbCrash("anr", "db", "b1", 100, "com.foo", dbT0+3600*s-30*s, 0, true)
	fresh := dbCrash("anr", "", "b1", 100, "com.foo", dbT0, 10, false)
	if AdoptDropbox([]*Crash{da, db}, fresh, jumps) {
		t.Fatalf("Adopt: equal deltas must stay unmatched")
	}
}

// Without a stored jump an equal distance keeps the first crash (behavior
// before issue #93).
func TestDropboxNoJumpEqualDeltaUnchanged(t *testing.T) {
	const s = int64(1e9)
	a := dbCrash("anr", "a", "b1", 100, "com.foo", dbT0-20*s, 10, false)
	b := dbCrash("anr", "b", "b1", 100, "com.foo", dbT0+20*s, 20, false)
	e := dbEntry("anr", 100, "com.foo", dbT0)
	if c, isNew := MatchDropboxBoot([]*Crash{a, b}, e, "b1", nil); isNew || c.ID != "a" {
		t.Fatalf("new=%v c=%+v", isNew, c)
	}
}

// The same-boot far fallback of Java crashes (issue #63) is unchanged: same
// pid and process in the boot match whatever the times, ANRs never do.
func TestDropboxFarFallbackUnchanged(t *testing.T) {
	const s = int64(1e9)
	j := dbCrash("java", "c1", "b1", 100, "com.foo", dbT0, 10, false)
	if c, isNew := MatchDropboxBoot([]*Crash{j}, dbEntry("java", 100, "com.foo", dbT0+7200*s), "b1", nil); isNew || c.ID != "c1" {
		t.Fatalf("java far fallback lost: new=%v", isNew)
	}
	a := dbCrash("anr", "c2", "b1", 100, "com.foo", dbT0, 10, false)
	if _, isNew := MatchDropboxBoot([]*Crash{a}, dbEntry("anr", 100, "com.foo", dbT0+7200*s), "b1", nil); !isNew {
		t.Fatalf("ANR must keep the time rule")
	}
}
