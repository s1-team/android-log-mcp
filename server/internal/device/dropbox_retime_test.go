package device

import (
	"strconv"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #63: after the device clock is set back, DropBox re-times entries
// dated after device now to the time of the next new entry. The same
// entry must not come back as a new crash.

func retimeEntry(ts, tag, proc string, pid string) string {
	return "========================================\n" + ts + " " + tag + " (text, 100 bytes)\nProcess: " + proc +
		"\nPID: " + pid + "\n\njava.lang.RuntimeException: boom\n\tat com.example.app.Main.run(Main.java:1)\n\n"
}

func retimePipe(t *testing.T) (*Pipeline, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 10, 11, 19, 40, 0, 0, time.UTC)
	st.Append([]*model.Record{{TimeNs: now.UnixNano(), Tag: "T", Msg: "m", Level: 'I'}})
	st.Flush()
	p := NewPipeline(st, "b1", time.UTC)
	dn := now
	p.DeviceNow = func() int64 { return dn.UnixNano() }
	return p, st, &dn
}

// an entry imported once, then re-timed to a set-back time, is not
// imported again
func TestDropboxRetimedEntryNotReimported(t *testing.T) {
	p, st, dn := retimePipe(t)
	p.ApplyDropbox("Drop box contents\n" + retimeEntry("2026-10-11 19:31:35", "data_app_crash", "com.example.app", "26273"))
	if n := len(LoadCrashes(st)); n != 1 {
		t.Fatalf("first import: %d crashes", n)
	}
	// clock set back a day; a new crash, and the old entry re-timed to it
	*dn = time.Date(2026, 10, 10, 19, 31, 0, 0, time.UTC)
	out := "Drop box contents\n" + retimeEntry("2026-10-10 19:30:40", "data_app_crash", "com.example.app", "26273") +
		retimeEntry("2026-10-10 19:30:40", "data_app_crash", "com.example.app", "26377")
	p.ApplyDropbox(out)
	p.ApplyDropbox(out)
	cs := LoadCrashes(st)
	if len(cs) != 2 {
		t.Fatalf("after the re-time: %d crashes, want 2 (one per pid)", len(cs))
	}
	for _, c := range cs {
		if c.PID == 26273 && time.Unix(0, c.TimeNs).UTC().Day() != 11 {
			t.Fatalf("pid 26273 moved to %s", time.Unix(0, c.TimeNs).UTC())
		}
	}
}

// an entry first fetched after DropBox re-timed it merges into the
// logcat crash of the same boot, pid and process
func TestDropboxRetimedEntryMergesSameProcess(t *testing.T) {
	p, st, dn := retimePipe(t)
	at := time.Date(2026, 10, 11, 19, 31, 35, 761e6, time.UTC)
	*dn = time.Date(1970, 1, 3, 8, 1, 0, 0, time.UTC)
	p.EmitCrash(&analysis.Crash{Kind: "java", Boot: "b1", TimeNs: at.UnixNano(), PID: 26273, Process: "com.example.app",
		Package: "com.example.app", Headline: "java.lang.RuntimeException: boom", Sources: []string{"logcat:crash"}})
	p.ApplyDropbox("Drop box contents\n" + retimeEntry("1970-01-03 08:00:25", "data_app_crash", "com.example.app", "26273"))
	cs := LoadCrashes(st)
	if len(cs) != 1 || cs[0].TimeNs != at.UnixNano() {
		t.Fatalf("crashes %+v", cs)
	}
	if !hasSource(cs[0], "dropbox:data_app_crash") {
		t.Fatalf("not merged: sources %v", cs[0].Sources)
	}
	// another boot's crash with that pid is not merged
	p2, st2, dn2 := retimePipe(t)
	*dn2 = *dn
	p2.EmitCrash(&analysis.Crash{Kind: "java", Boot: "b0", TimeNs: at.UnixNano(), PID: 26273, Process: "com.example.app", Sources: []string{"logcat:crash"}})
	p2.ApplyDropbox("Drop box contents\n" + retimeEntry("1970-01-03 08:00:25", "data_app_crash", "com.example.app", "26273"))
	if n := len(LoadCrashes(st2)); n != 2 {
		t.Fatalf("other boot: %d crashes, want 2", n)
	}
}

// two ANRs of one surviving process far apart stay two
func TestDropboxAnrSamePidFarApart(t *testing.T) {
	p, st, _ := retimePipe(t)
	anr := func(ts, subject string) string {
		return "========================================\n" + ts + " data_app_anr (text, 100 bytes)\nProcess: com.example.app\nPID: 500\nSubject: " + subject + "\n\nCPU usage\n\n"
	}
	p.ApplyDropbox("Drop box contents\n" + anr("2026-10-11 10:00:00", "Input dispatching timed out (first)"))
	p.ApplyDropbox("Drop box contents\n" + anr("2026-10-11 19:00:00", "Input dispatching timed out (second)"))
	if n := len(LoadCrashes(st)); n != 2 {
		t.Fatalf("%d ANRs, want 2", n)
	}
}

// a key written by older versions (tag|timeNs) still keeps the entry out
func TestDropboxLegacyTimeKeyHonoured(t *testing.T) {
	p, st, _ := retimePipe(t)
	ts := time.Date(2026, 10, 11, 19, 31, 35, 0, time.UTC).UnixNano()
	st.AppendAux("dropbox_seen", seenLine{Key: "data_app_crash|" + strconv.FormatInt(ts, 10), HostNs: time.Now().UnixNano()})
	p.ApplyDropbox("Drop box contents\n" + retimeEntry("2026-10-11 19:31:35", "data_app_crash", "com.example.app", "26273"))
	if n := len(LoadCrashes(st)); n != 0 {
		t.Fatalf("%d crashes imported despite the legacy key", n)
	}
}

func hasSource(c *analysis.Crash, s string) bool {
	for _, x := range c.Sources {
		if x == s {
			return true
		}
	}
	return false
}
