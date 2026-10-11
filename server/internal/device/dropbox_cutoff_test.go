package device

import (
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// The DropBox import cutoff is device now − retention, not the highest
// device time ever stored (issue #53).

func dropboxOut(stamps ...string) string {
	var b strings.Builder
	b.WriteString("Drop box contents: entries\n")
	for i, ts := range stamps {
		b.WriteString("========================================\n" + ts + " data_app_crash (text, 100 bytes)\nProcess: com.example.app" + string(rune('a'+i)) + "\nPID: 42\n\njava.lang.RuntimeException: boom " + ts + "\n\tat com.example.app.Main.run(Main.java:1)\n\n")
	}
	return b.String()
}

func crashTimes(st *store.Store) []string {
	var out []string
	for _, c := range LoadCrashes(st) {
		out = append(out, time.Unix(0, c.TimeNs).UTC().Format("2006-01-02 15:04:05"))
	}
	return out
}

func cutoffPipe(t *testing.T, newest time.Time) (*Pipeline, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Append([]*model.Record{{TimeNs: newest.UnixNano(), Tag: "T", Msg: "m", Level: 'I'}})
	st.Flush()
	p := NewPipeline(st, "b1", time.UTC)
	now := newest
	p.DeviceNow = func() int64 { return now.UnixNano() }
	return p, st, &now
}

// A clock reset to 1970 (more than the retention back): new entries are
// imported once; entries from before the reset, already imported, are not
// imported again, and entries past retention are not imported.
func TestDropboxCutoffAfter1970Reset(t *testing.T) {
	p, st, now := cutoffPipe(t, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	before := []string{"2026-10-10 09:00:00", "2026-10-10 11:00:00"}
	p.ApplyDropbox(dropboxOut(before...))
	if got := crashTimes(st); len(got) != 2 {
		t.Fatalf("before the reset: %q", got)
	}
	*now = time.Date(1970, 1, 1, 1, 0, 5, 0, time.UTC)
	out := dropboxOut(append(before, "1969-12-30 00:00:00", "1970-01-01 00:59:00")...)
	p.ApplyDropbox(out)
	p.ApplyDropbox(out) // idempotent
	got := crashTimes(st)
	if len(got) != 3 || got[2] != "1970-01-01 00:59:00" {
		t.Fatalf("after the reset: %q", got)
	}
}

// +1 year, then the clock is restored: entries of the restored clock are
// imported (the old cutoff stayed a year ahead), the +1 y entry once, and
// entries past retention are not imported.
func TestDropboxCutoffYearAheadUndone(t *testing.T) {
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	p, st, now := cutoffPipe(t, base)
	ahead := base.AddDate(1, 0, 0)
	*now = ahead
	st.Append([]*model.Record{{TimeNs: ahead.UnixNano(), Tag: "T", Msg: "a year ahead", Level: 'I'}})
	p.ApplyDropbox(dropboxOut("2027-10-10 11:59:00"))
	*now = base.Add(time.Hour)
	out := dropboxOut("2027-10-10 11:59:00", "2026-10-08 12:00:00", "2026-10-10 12:30:00")
	p.ApplyDropbox(out)
	p.ApplyDropbox(out)
	got := crashTimes(st)
	if len(got) != 2 || got[0] != "2027-10-10 11:59:00" || got[1] != "2026-10-10 12:30:00" {
		t.Fatalf("crashes %q", got)
	}
}
