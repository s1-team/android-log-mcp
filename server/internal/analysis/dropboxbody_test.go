package analysis

// Issue #152: DropBox re-dates entries after a back-jump; an ANR entry's
// body still carries the time of its own trace dump, which the matching
// uses. Tests go through ParseDropbox and MatchDropboxBoot.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const bodyFmt = "2006-01-02 15:04:05"

// anrDump is `dumpsys dropbox --print` text with one ANR entry whose header
// says hdr and whose trace header says body (in the zone loc).
func anrDump(hdr, bodyTime string, pid int) string {
	return fmt.Sprintf("Drop box contents: 1 entries\n\n========================================\n%s data_app_anr (compressed text, 9 bytes)\n"+
		"Process: com.foo\nPID: %d\nSubject: Input dispatching timed out\n\n----- pid 1995 at 2000-01-01 00:00:00 -----\nCmd line: system_server\n\n"+
		"----- pid %d at %s -----\nCmd line: com.foo\n\n----- pid 2763 at 2000-01-01 00:00:09 -----\n", hdr, pid, pid, bodyTime)
}

func parseOne(t *testing.T, out string, loc *time.Location) DropboxEntry {
	t.Helper()
	es := ParseDropbox(out, loc)
	if len(es) != 1 {
		t.Fatalf("parsed %d entries", len(es))
	}
	return es[0]
}

func TestParseDropboxANRBodyTime(t *testing.T) {
	t.Parallel()
	sh := time.FixedZone("CST", 8*3600)
	e := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40", 9692), sh)
	want := time.Date(2026, 10, 16, 14, 31, 40, 0, sh).UnixNano()
	if e.BodyTimeNs != want {
		t.Fatalf("BodyTimeNs = %d, want %d (PID's own trace header, device zone)", e.BodyTimeNs, want)
	}
	if e.RealTimeNs() != want {
		t.Fatal("RealTimeNs must be the body time")
	}
	// fractional seconds, other zone: read in the zone given
	utc := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40.250", 9692), time.UTC)
	if utc.BodyTimeNs != time.Date(2026, 10, 16, 14, 31, 40, 250e6, time.UTC).UnixNano() {
		t.Fatalf("fraction/zone: %d", utc.BodyTimeNs)
	}
	// parse failure, no header of the PID, other tags: header time
	for name, out := range map[string]string{
		"unparseable": anrDump("2026-10-15 14:46:11", "x", 9692),
		"bad date":    anrDump("2026-10-15 14:46:11", "2026-13-45 99:99:99", 9692),
		"other pid":   strings.Replace(anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40", 9692), "pid 9692 at", "pid 1111 at", 1),
		"java entry":  strings.Replace(anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40", 9692), "data_app_anr", "data_app_crash", 1),
	} {
		e := parseOne(t, out, sh)
		if e.BodyTimeNs != 0 || e.RealTimeNs() != e.TimeNs {
			t.Fatalf("%s: BodyTimeNs=%d RealTimeNs=%d TimeNs=%d, want header time", name, e.BodyTimeNs, e.RealTimeNs(), e.TimeNs)
		}
	}
}

// First read of an entry the device already re-dated after a back-jump:
// header time = the next new entry's time, nowhere near the ANR.
func TestDropboxRedatedEntryFirstReadMergesByBodyTime(t *testing.T) {
	t.Parallel()
	const s = int64(1e9)
	loc := time.UTC
	anrAt := time.Date(2026, 10, 16, 14, 31, 39, 171e6, loc).UnixNano()
	list := []*Crash{
		dbCrash("anr", "anr10", "b1", 9692, "com.foo", anrAt, 10, false),
		dbCrash("anr", "anr9", "b1", 7814, "com.foo", anrAt-27*60*s, 5, false),
	}
	// the clock went back one day after the ANR (jump stored after seq 10)
	back := []ClockShift{{Seq: 20, DeltaNs: -86400 * s}}
	e := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40", 9692), loc)
	for name, jumps := range map[string][]ClockShift{"no stored jump": nil, "back-jump stored (#135 shift)": back} {
		c, isNew := MatchDropboxBoot(list, e, "b1", jumps)
		if c == nil || isNew || c.ID != "anr10" {
			t.Fatalf("%s: got %+v new=%v, want merge into anr10", name, c, isNew)
		}
	}
	// without the body time (what 0.8.0 had) it is a new ANR
	e.BodyTimeNs = 0
	if _, isNew := MatchDropboxBoot(list, e, "b1", nil); !isNew {
		t.Fatal("premise: header time alone does not match")
	}
}

// Coexistence with the jump shift of #93/#135: the body time is compared
// raw and shifted exactly like the header time, and the header time stays
// a candidate.
func TestDropboxBodyTimeWithJumpShift(t *testing.T) {
	t.Parallel()
	const s = int64(1e9)
	loc := time.UTC
	crashAt := time.Date(2026, 10, 16, 14, 31, 39, 0, loc).UnixNano()
	back := []ClockShift{{Seq: 20, DeltaNs: -86400 * s}}
	for _, tc := range []struct {
		name     string
		hdr, bod string
		jumps    []ClockShift
		want     bool
	}{
		{"body time raw inside", "2026-10-15 14:46:11", "2026-10-16 14:32:30", back, true},
		{"body time raw edge outside", "2026-10-15 14:46:11", "2026-10-16 14:33:39", back, false},
		// the entry was dumped after the jump: its body time is the shifted one
		{"body time on the shifted clock", "2026-10-15 14:46:11", "2026-10-15 14:31:50", back, true},
		{"body time on the shifted clock, jump not stored", "2026-10-15 14:46:11", "2026-10-15 14:31:50", nil, false},
		// header time still works when the body time is far off
		{"header candidate", "2026-10-15 14:31:45", "2026-10-10 00:00:00", back, true},
		{"header candidate, no jump", "2026-10-16 14:31:45", "2026-10-10 00:00:00", nil, true},
		{"neither near", "2026-10-15 14:46:11", "2026-10-10 00:00:00", back, false},
	} {
		list := []*Crash{dbCrash("anr", "a1", "b1", 9692, "com.foo", crashAt, 10, false)}
		e := parseOne(t, anrDump(tc.hdr, tc.bod, 9692), loc)
		c, isNew := MatchDropboxBoot(list, e, "b1", tc.jumps)
		if got := c != nil && !isNew; got != tc.want {
			t.Errorf("%s: merged=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// Two real ANRs of one pid stay two even when DropBox gave both entries the
// same re-dated header time.
func TestDropboxBodyTimeKeepsSamePidANRsApart(t *testing.T) {
	t.Parallel()
	const s = int64(1e9)
	loc := time.UTC
	t1 := time.Date(2026, 10, 16, 10, 0, 0, 0, loc).UnixNano()
	list := []*Crash{
		dbCrash("anr", "a1", "b1", 77, "com.foo", t1, 10, false),
		dbCrash("anr", "a2", "b1", 77, "com.foo", t1+40*s, 11, false),
	}
	e2 := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 10:00:41", 77), loc)
	c, isNew := MatchDropboxBoot(list, e2, "b1", nil)
	if c == nil || isNew || c.ID != "a2" {
		t.Fatalf("second ANR entry: %+v new=%v, want a2", c, isNew)
	}
	e1 := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 09:59:59", 77), loc)
	if c, isNew := MatchDropboxBoot(list, e1, "b1", nil); c == nil || isNew || c.ID != "a1" {
		t.Fatalf("first ANR entry: %+v new=%v, want a1", c, isNew)
	}
}

// A new ANR built from an entry carries the body time, not the re-dated one.
func TestDropboxNewANRUsesBodyTime(t *testing.T) {
	t.Parallel()
	e := parseOne(t, anrDump("2026-10-15 14:46:11", "2026-10-16 14:31:40", 9692), time.UTC)
	c, isNew := MatchDropboxBoot(nil, e, "b1", nil)
	if c == nil || !isNew || c.TimeNs != time.Date(2026, 10, 16, 14, 31, 40, 0, time.UTC).UnixNano() {
		t.Fatalf("new ANR: %+v", c)
	}
}
