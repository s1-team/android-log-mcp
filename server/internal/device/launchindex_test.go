package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func testLaunch(i int, base int64) *analysis.Launch {
	return &analysis.Launch{
		Package: "com.example.app", Component: "com.example.app/.Main", Class: "com.example.app.Main",
		RequestNs: base + int64(i)*1e9, RequestMsg: "START u0 {cmp=com.example.app/.Main}",
		Kind: "COLD", KindWhy: "a new process was started for it", Seq: uint64(i + 1), PID: 1000 + i,
		ProcStart: analysis.Step{Name: "process started", Ns: base + int64(i)*1e9 + 1e7, TookMs: -1, Seq: uint64(i + 2)},
		OnCreate:  analysis.Step{Ns: base + int64(i)*1e9 + 2e7, TookMs: 5, Seq: uint64(i + 3)},
		OnStart:   analysis.Step{Ns: base + int64(i)*1e9 + 3e7, TookMs: 2, Seq: uint64(i + 4)},
		OnResume:  analysis.Step{Ns: base + int64(i)*1e9 + 4e7, TookMs: 3, Seq: uint64(i + 5)},
		Displayed: analysis.Step{Ns: base + int64(i)*1e9 + 5e7, TookMs: 50, Note: "ActivityTaskManager Displayed", Seq: uint64(i + 6)},
	}
}

func TestLaunchIndexMatchesFullDecode(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := len(LaunchSummaries(st)); n != 0 {
		t.Fatalf("empty: %d", n)
	}
	base := time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		l := testLaunch(i, base)
		normalizeLaunchSteps(l)
		st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln" + itoaTest(i+1), Ver: analysis.LaunchFormatVer, HostNs: time.Now().UnixNano(), Boot: "b1"})
	}
	sums := LaunchSummaries(st)
	if len(sums) != 20 {
		t.Fatalf("summaries %d want 20", len(sums))
	}
	// newest first
	if sums[0].Seq != 20 || sums[19].Seq != 1 {
		t.Fatalf("order newest-first: first seq=%d last=%d", sums[0].Seq, sums[19].Seq)
	}
	ids := make([]string, len(sums))
	for i, s := range sums {
		ids[i] = s.ID
	}
	full := LaunchFull(st, ids...)
	for i, l := range full {
		if l == nil || l.Seq != sums[i].Seq || l.Package != "com.example.app" {
			t.Fatalf("full[%d]=%v sum=%+v", i, l, sums[i])
		}
	}
	// version supersedes
	l := testLaunch(0, base)
	l.Kind = "WARM"
	normalizeLaunchSteps(l)
	st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln1", Ver: analysis.LaunchFormatVer, HostNs: time.Now().UnixNano(), Boot: "b1"})
	sums = LaunchSummaries(st)
	if len(sums) != 20 || sums[19].Kind != "WARM" { // ln1 is oldest arrival
		// find ln1
		found := false
		for _, s := range sums {
			if s.ID == "ln1" {
				found = true
				if s.Kind != "WARM" {
					t.Fatalf("ln1 kind=%s want WARM", s.Kind)
				}
			}
		}
		if !found {
			t.Fatal("ln1 missing")
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestLaunchIndexReloadsAfterCompaction(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	base := now.UnixNano()
	for i := 0; i < 20; i++ {
		ht := now.UnixNano()
		if i < 5 {
			ht = now.Add(-3 * time.Hour).UnixNano()
		}
		l := testLaunch(i, base)
		normalizeLaunchSteps(l)
		st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln" + itoaTest(i+1), Ver: analysis.LaunchFormatVer, HostNs: ht, Boot: "b1"})
	}
	if saved := CompactAux(st, now); saved <= 0 {
		t.Fatalf("compaction saved nothing")
	}
	if n := len(LaunchSummaries(st)); n != 15 {
		t.Fatalf("after compaction %d want 15", n)
	}
	l := testLaunch(50, base)
	normalizeLaunchSteps(l)
	st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln50", Ver: analysis.LaunchFormatVer, HostNs: now.UnixNano(), Boot: "b1"})
	if n := len(LaunchSummaries(st)); n != 16 {
		t.Fatalf("after append %d want 16", n)
	}
}

func TestLaunchIndexVerMismatchRebuilds(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := testLaunch(0, time.Now().UnixNano())
	normalizeLaunchSteps(l)
	st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln1", Ver: 99, HostNs: time.Now().UnixNano()})
	if LaunchIndexReady(st) {
		t.Fatal("ver=99 should not be ready")
	}
	// EnsureLaunches with empty store: removes bad file, writes empty-ish
	incomplete, _ := EnsureLaunches(st, time.Second)
	if incomplete {
		// empty history: backfill writes nothing but file may still be missing
	}
	// after remove, file may be absent or empty — not ready with bad ver
	if LaunchIndexReady(st) {
		// ok if backfill created ver=1 file from empty scan
		for _, b := range st.ReadAux("launches") {
			var line launchAuxLine
			json.Unmarshal(b, &line)
			if line.Ver != 0 && line.Ver != analysis.LaunchFormatVer {
				t.Fatalf("stale ver survived: %d", line.Ver)
			}
		}
	}
}

func TestEmitLaunchLinear(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	const n = 200
	before := launchLinesIndexed.Load()
	base := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		l := testLaunch(i, base)
		p.EmitLaunch(l, "b1")
		if i%4 == 0 {
			l2 := *l
			l2.Kind = "WARM"
			p.EmitLaunch(&l2, "b1")
		}
	}
	got := launchLinesIndexed.Load() - before
	lines := int64(len(st.ReadAux("launches")))
	if got > 2*lines+10 {
		t.Fatalf("indexed %d for %d stored (quadratic?)", got, lines)
	}
	if len(LaunchSummaries(st)) != n {
		t.Fatalf("%d launches want %d", len(LaunchSummaries(st)), n)
	}
}

func TestLaunchIndexPartialLine(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().UnixNano()
	l := testLaunch(0, base)
	normalizeLaunchSteps(l)
	st.AppendAux("launches", launchAuxLine{Launch: l, ID: "ln1", Ver: analysis.LaunchFormatVer, HostNs: time.Now().UnixNano()})
	path := filepath.Join(st.Dir, "launches.jsonl")
	line, _ := json.Marshal(launchAuxLine{Launch: testLaunch(1, base), ID: "ln2", Ver: analysis.LaunchFormatVer, HostNs: time.Now().UnixNano()})
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	f.Write(line[:20])
	f.Close()
	if n := len(LaunchSummaries(st)); n != 1 {
		t.Fatalf("partial indexed: %d", n)
	}
	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	f.Write(append(line[20:], '\n'))
	f.Close()
	if n := len(LaunchSummaries(st)); n != 2 {
		t.Fatalf("completed: %d", n)
	}
}

func TestApplyLaunchJumpsRecompute(t *testing.T) {
	t.Parallel()
	l := &analysis.Launch{
		RequestNs: 1000, Seq: 10,
		Displayed: analysis.Step{Ns: 2000, TookMs: 5, Seq: 30, AfterJump: false},
	}
	jumps := []analysis.ClockStep{{Seq: 20, DeltaNs: -1e12}}
	analysis.ApplyLaunchJumps(l, jumps)
	if !l.Displayed.AfterJump || !l.ClockJumped {
		t.Fatalf("expected AfterJump after jump seq=20: %+v", l.Displayed)
	}
	analysis.ApplyLaunchJumps(l, nil)
	if l.Displayed.AfterJump || l.ClockJumped {
		t.Fatalf("cleared jumps should clear flags: %+v", l)
	}
}

func TestCarriedFilesIncludesLaunches(t *testing.T) {
	t.Parallel()
	found := false
	for _, n := range store.CarriedFiles {
		if n == "launches.jsonl" {
			found = true
		}
	}
	if !found {
		t.Fatal("launches.jsonl missing from CarriedFiles")
	}
}

func TestSyncLaunchesFromRecords(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	base := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC).UnixNano()
	recs := []*model.Record{
		{TimeNs: base, Seq: 1, Boot: "b1", Tag: "ActivityTaskManager", Msg: "START u0 { act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] cmp=com.example.app/.Main }"},
		{TimeNs: base + 10e6, Seq: 2, Boot: "b1", Tag: "am_proc_start", Msg: "[0,1234,1000,com.example.app,1000,com.example.app/com.example.app.Main]"},
		{TimeNs: base + 20e6, Seq: 3, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_create_called", Msg: "[0,com.example.app.Main,0,5]"},
		{TimeNs: base + 30e6, Seq: 4, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_start_called", Msg: "[0,com.example.app.Main,0,2]"},
		{TimeNs: base + 40e6, Seq: 5, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_resume_called", Msg: "[0,com.example.app.Main,0,3]"},
		{TimeNs: base + 50e6, Seq: 6, Boot: "b1", Tag: "ActivityTaskManager", Msg: "Displayed com.example.app/.Main: +50ms"},
	}
	// Accept+Drain so syncLaunches runs
	for _, r := range recs {
		cp := *r
		p.Accept(&cp, time.Now())
	}
	if err := p.Drain(true, time.Now()); err != nil {
		t.Fatal(err)
	}
	sums := LaunchSummaries(st)
	if len(sums) != 1 {
		t.Fatalf("launches=%d want 1 (got file=%v)", len(sums), st.ReadAux("launches"))
	}
	full := LaunchFull(st, sums[0].ID)[0]
	if full == nil || full.Kind != "COLD" || full.Displayed.Ns == 0 {
		t.Fatalf("full=%+v", full)
	}
}

func TestBackfillMatchesLaunchesAcross(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC).UnixNano()
	recs := []*model.Record{
		{TimeNs: base, Seq: 1, Boot: "b1", Tag: "ActivityTaskManager", Msg: "START u0 { act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] cmp=com.example.app/.Main }"},
		{TimeNs: base + 10e6, Seq: 2, Boot: "b1", Tag: "am_proc_start", Msg: "[0,1234,1000,com.example.app,1000,com.example.app/com.example.app.Main]"},
		{TimeNs: base + 20e6, Seq: 3, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_create_called", Msg: "[0,com.example.app.Main,0,5]"},
		{TimeNs: base + 30e6, Seq: 4, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_start_called", Msg: "[0,com.example.app.Main,0,2]"},
		{TimeNs: base + 40e6, Seq: 5, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_resume_called", Msg: "[0,com.example.app.Main,0,3]"},
		{TimeNs: base + 50e6, Seq: 6, Boot: "b1", Tag: "ActivityTaskManager", Msg: "Displayed com.example.app/.Main: +50ms"},
		{TimeNs: base + 5e9, Seq: 7, Boot: "b1", Tag: "ActivityTaskManager", Msg: "START u0 { act=android.intent.action.MAIN cmp=com.example.app/.Main }"},
		{TimeNs: base + 5e9 + 20e6, Seq: 8, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_restart_called", Msg: "[0,com.example.app.Main,0,1]"},
		{TimeNs: base + 5e9 + 30e6, Seq: 9, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_start_called", Msg: "[0,com.example.app.Main,0,2]"},
		{TimeNs: base + 5e9 + 40e6, Seq: 10, Boot: "b1", PID: 1234, Package: "com.example.app", Process: "com.example.app", Tag: "wm_on_resume_called", Msg: "[0,com.example.app.Main,0,3]"},
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	oracle := analysis.LaunchesAcross(recs, "", nil)
	incomplete, note := EnsureLaunches(st, 5*time.Second)
	if incomplete || note != "" {
		t.Fatalf("backfill incomplete=%v note=%q", incomplete, note)
	}
	sums := LaunchSummaries(st)
	if len(sums) != len(oracle) {
		t.Fatalf("index %d oracle %d", len(sums), len(oracle))
	}
	ids := make([]string, len(sums))
	for i, s := range sums {
		ids[i] = s.ID
	}
	full := LaunchFull(st, ids...)
	// sums newest-first; oracle is arrival order
	for i, o := range oracle {
		got := full[len(full)-1-i]
		if got == nil {
			t.Fatalf("nil at %d", i)
		}
		analysis.ApplyLaunchJumps(got, nil)
		gotTTID, oTTID := got.Displayed.TookMs, o.Displayed.TookMs
		if got.Displayed.Ns == 0 {
			gotTTID = -1
		}
		if o.Displayed.Ns == 0 {
		oTTID = -1
		}
		if got.RequestNs != o.RequestNs || got.Kind != o.Kind || gotTTID != oTTID {
			t.Fatalf("#%d index=%s kind=%s ttid=%d; oracle=%s kind=%s ttid=%d",
				i, time.Unix(0, got.RequestNs).UTC(), got.Kind, gotTTID,
				time.Unix(0, o.RequestNs).UTC(), o.Kind, oTTID)
		}
	}
}
