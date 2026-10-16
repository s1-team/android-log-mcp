package device

import (
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #93: the pipeline hands the stored clock jumps of its boot session
// (not the "possible" ones) to the DropBox matching.

// The trace header carries no time here: these cases test the header time (#93); body times are tested in #152.
func anrEntry(ts string) string {
	return "========================================\n" + ts + " data_app_anr (text, 100 bytes)\nProcess: com.example.app\nPID: 4321\n" +
		"Subject: Input dispatching timed out\n\n----- pid 4321 at (no time) -----\nCmd line: com.example.app\n\n"
}

func TestDropboxMatchUsesStoredClockJumps(t *testing.T) {
	t.Parallel()
	crashAt := time.Date(2026, 10, 11, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		jump     store.ClockJump
		sessions []store.Session
		entry    string // entry time, 1 h after the crash plus 5 s
		want     int    // crashes after the fetch
	}{
		{"measured jump after the crash", store.ClockJump{Seq: 2, DeltaNs: int64(3600e9), Source: "measured"}, nil, "2026-10-11 20:00:05", 1},
		{"lines jump after the crash", store.ClockJump{Seq: 2, DeltaNs: int64(3600e9), Source: "lines"}, nil, "2026-10-11 20:00:05", 1},
		{"possible jump is ignored", store.ClockJump{Seq: 2, DeltaNs: int64(3600e9), Source: "possible"}, nil, "2026-10-11 20:00:05", 2},
		{"no jump", store.ClockJump{}, nil, "2026-10-11 20:00:05", 2},
		{"jump in an earlier boot is ignored", store.ClockJump{Seq: 2, DeltaNs: int64(3600e9), Source: "measured"},
			[]store.Session{{Boot: "b0", FirstSeq: 1}, {Boot: "b1", FirstSeq: 3}}, "2026-10-11 20:00:05", 2},
		{"jump in this boot", store.ClockJump{Seq: 2, DeltaNs: int64(3600e9), Source: "measured"},
			[]store.Session{{Boot: "b0", FirstSeq: 1}, {Boot: "b1", FirstSeq: 1}}, "2026-10-11 20:00:05", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			var recs []*model.Record
			for i := 0; i < 3; i++ {
				recs = append(recs, &model.Record{TimeNs: crashAt.Add(time.Duration(i) * time.Second).UnixNano(), Tag: "T", Msg: "m", Level: 'I'})
			}
			st.Append(recs)
			st.Flush()
			if tc.jump.Source != "" {
				st.AddClockJump(tc.jump)
			}
			if tc.sessions != nil {
				st.UpdateMeta(func(m *store.Meta) { m.Sessions = tc.sessions })
			}
			p := NewPipeline(st, "b1", time.UTC)
			dn := crashAt.Add(2 * time.Hour)
			p.DeviceNow = func() int64 { return dn.UnixNano() }
			p.EmitCrash(&analysis.Crash{Kind: "anr", Boot: "b1", TimeNs: crashAt.UnixNano(), Seq: 1, PID: 4321, Process: "com.example.app",
				Package: "com.example.app", Headline: "ANR", Sources: []string{"logcat:crash"}})
			p.ApplyDropbox("Drop box contents\n" + anrEntry(tc.entry))
			cs := LoadCrashes(st)
			if len(cs) != tc.want {
				t.Fatalf("%d crashes, want %d", len(cs), tc.want)
			}
			if tc.want == 1 && !hasSource(cs[0], "dropbox:data_app_anr") {
				t.Fatalf("not merged: %v", cs[0].Sources)
			}
		})
	}
}
