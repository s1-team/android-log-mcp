package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func TestCompactAuxKeepsLastVersionsAndDropsExpired(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	now := time.Now()
	old := now.Add(-48 * time.Hour).UnixNano()
	recent := now.Add(-time.Hour).UnixNano()
	st.Append([]*model.Record{{TimeNs: now.Add(-2 * time.Hour).UnixNano(), Tag: "T", Msg: "m", Level: 'I'}})

	// procs: many updates of a few entries; one ended long ago
	for i := 0; i < 20; i++ {
		st.AppendAux("procs", procs.Proc{ID: "p1", Boot: "b", PID: 1, Name: "a", StartNs: recent, SeenNs: int64(i)})
		st.AppendAux("procs", procs.Proc{ID: "p2", Boot: "b", PID: 2, Name: "b", StartNs: recent})
	}
	st.AppendAux("procs", procs.Proc{ID: "gone", Boot: "b0", PID: 3, Name: "c", StartNs: old, EndNs: old + 1})
	st.AppendAux("procs", procs.Proc{ID: "p2", Boot: "b", PID: 2, Name: "b", StartNs: recent, EndNs: recent + 5, EndReason: "died"})
	// crashes: superseded versions, one expired
	for i := 0; i < 5; i++ {
		st.AppendAux("crashes", analysis.Crash{ID: "cr1", TimeNs: recent, Sources: []string{fmt.Sprint("v", i)}})
	}
	st.AppendAux("crashes", analysis.Crash{ID: "cr0", TimeNs: old})
	st.AppendAux("crashes", analysis.Crash{ID: "anr1", TimeNs: recent + 1})
	// dropbox_seen: one expired, one duplicate
	for _, k := range []string{fmt.Sprintf("%q", fmt.Sprintf("data_app_crash|%d", old)), fmt.Sprintf("%q", fmt.Sprintf("data_app_crash|%d", recent)), fmt.Sprintf("%q", fmt.Sprintf("data_app_crash|%d", recent))} {
		st.AppendAux("dropbox_seen", json.RawMessage(k))
	}
	os.WriteFile(filepath.Join(st.Dir, "crashes.jsonl.tmp"), []byte("x"), 0o644)
	oldT := now.Add(-time.Hour)
	os.Chtimes(filepath.Join(st.Dir, "crashes.jsonl.tmp"), oldT, oldT)

	wantProcs := map[string]procs.Proc{}
	for _, p := range LoadProcs(st) {
		if p.ID != "gone" {
			wantProcs[p.ID] = p
		}
	}
	var wantCrashes []string
	for _, c := range LoadCrashes(st) {
		if c.ID != "cr0" {
			b, _ := json.Marshal(c)
			wantCrashes = append(wantCrashes, string(b))
		}
	}
	size := func(n string) int64 { fi, _ := os.Stat(filepath.Join(st.Dir, n)); return fi.Size() }
	before := size("procs.jsonl") + size("crashes.jsonl") + size("dropbox_seen.jsonl")

	saved := CompactAux(st, now)
	after := size("procs.jsonl") + size("crashes.jsonl") + size("dropbox_seen.jsonl")
	if saved <= 0 || after != before-saved {
		t.Fatalf("saved %d, %d → %d", saved, before, after)
	}
	gotProcs := map[string]procs.Proc{}
	for _, p := range LoadProcs(st) {
		gotProcs[p.ID] = p
	}
	if !reflect.DeepEqual(gotProcs, wantProcs) || len(st.ReadAux("procs")) != 2 {
		t.Fatalf("procs after compaction: %+v", gotProcs)
	}
	var gotCrashes []string
	for _, c := range LoadCrashes(st) {
		b, _ := json.Marshal(c)
		gotCrashes = append(gotCrashes, string(b))
	}
	if strings.Join(gotCrashes, "\n") != strings.Join(wantCrashes, "\n") || len(st.ReadAux("crashes")) != 2 {
		t.Fatalf("crashes after compaction:\n%s", strings.Join(gotCrashes, "\n"))
	}
	if n := len(st.ReadAux("dropbox_seen")); n != 1 {
		t.Fatalf("dropbox_seen keeps %d", n)
	}
	if _, err := os.Stat(filepath.Join(st.Dir, "crashes.jsonl.tmp")); !os.IsNotExist(err) {
		t.Fatal("stale tmp file not removed")
	}
	if CompactAux(st, now) != 0 {
		t.Fatal("second compaction should find nothing")
	}
}

func TestLowDiskPausesAndResumes(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	free := uint64(100 << 20)
	g := &store.DiskGuard{MinFree: 512 << 20, Free: func(string) (uint64, error) { return free, nil }}
	d := newDevice(nil, st, "d")
	d.disk = g
	d.state = "device"
	g.Check(st.Dir)
	s := d.Status()
	if !strings.Contains(s.Paused, "recording paused") || !strings.Contains(s.Paused, "100 MiB free") {
		t.Fatalf("status while low: %+v", s)
	}
	st.Append([]*model.Record{{TimeNs: 5e9, Tag: "T", Msg: "before", Level: 'I'}})
	recordPause(st)
	recordPause(st) // idempotent while the gap is open
	m := st.Meta()
	if len(m.Gaps) != 1 || m.Gaps[0].ToNs != -1 || m.Gaps[0].FromNs != 5e9 || m.Gaps[0].Reason != PauseGapReason {
		t.Fatalf("gaps %+v", m.Gaps)
	}
	free = 600 << 20 // above MinFree, below the resume threshold
	g.Check(st.Dir)
	if d.Status().Paused == "" {
		t.Fatal("resumed without hysteresis")
	}
	free = 2 << 30
	if paused, changed := g.Check(st.Dir); paused || !changed || d.Status().Paused != "" {
		t.Fatal("did not resume")
	}
	// a follower (not the writer) never reports a pause of its own
	f, _ := store.OpenDir(st.Dir, "d", store.Config{}, true)
	t.Cleanup(f.Close)
	fd := newDevice(nil, f, "d")
	fd.disk, fd.state = g, "device"
	free = 1 << 20
	g.Check(st.Dir)
	if fd.Status().Paused != "" {
		t.Fatal("follower shows writer pause")
	}
}

func TestManagerDiskDefaultsFromEnv(t *testing.T) {
	m := NewManager(t.TempDir(), store.Config{})
	if m.MaxTotalBytes != 2<<30 || m.Disk.MinFree != 512<<20 {
		t.Fatalf("defaults %d %d", m.MaxTotalBytes, m.Disk.MinFree)
	}
	t.Setenv("ANDROID_LOG_MCP_MAX_TOTAL_MB", "1024")
	t.Setenv("ANDROID_LOG_MCP_MIN_FREE_MB", "0")
	m = NewManager(t.TempDir(), store.Config{})
	if m.MaxTotalBytes != 1<<30 || m.Disk.MinFree != 0 {
		t.Fatalf("env %d %d", m.MaxTotalBytes, m.Disk.MinFree)
	}
}

// The aux cleanup follows the configured retention (ANDROID_LOG_MCP_MAX_AGE).
func TestCompactAuxFollowsConfiguredRetention(t *testing.T) {
	now := time.Now()
	twoDays := now.Add(-48 * time.Hour).UnixNano()
	for _, tc := range []struct {
		maxAge time.Duration
		kept   int
	}{{24 * time.Hour, 0}, {7 * 24 * time.Hour, 1}} {
		st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: tc.maxAge, IdleRelease: -1})
		if err != nil {
			t.Fatal(err)
		}
		st.Append([]*model.Record{{TimeNs: twoDays, Tag: "T", Msg: "m", Level: 'I'}})
		st.AppendAux("crashes", analysis.Crash{ID: "cr1", TimeNs: twoDays})
		st.AppendAux("procs", procs.Proc{ID: "p", Boot: "b", PID: 1, Name: "x", StartNs: twoDays - 2*int64(time.Hour), EndNs: twoDays - int64(2*time.Hour) + 1})
		st.AppendAux("procs", procs.Proc{ID: "q", Boot: "b", PID: 2, Name: "y", StartNs: now.UnixNano()})
		st.AppendAux("dropbox_seen", json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("data_app_crash|%d", twoDays))))
		CompactAux(st, now)
		if n := len(st.ReadAux("crashes")); n != tc.kept {
			t.Errorf("MaxAge %v: %d crashes kept, want %d", tc.maxAge, n, tc.kept)
		}
		if n := len(st.ReadAux("dropbox_seen")); n != tc.kept {
			t.Errorf("MaxAge %v: %d dropbox keys kept, want %d", tc.maxAge, n, tc.kept)
		}
		// the ended process is dropped only outside the retention window
		if n := len(st.ReadAux("procs")); n != 1+tc.kept {
			t.Errorf("MaxAge %v: %d procs kept, want %d", tc.maxAge, n, 1+tc.kept)
		}
		st.Close()
	}
}

// DropBox entries older than the retention window (relative to the newest
// stored line) are not imported, so cleaned-up dropbox_seen keys cannot
// bring old crashes back as new ones.
func TestApplyDropboxSkipsEntriesOutsideRetention(t *testing.T) {
	loc := time.UTC
	entry := func(ts string) string {
		return "========================================\n" + ts + " data_app_crash (text, 100 bytes)\nProcess: com.example.app\nPID: 42\n\njava.lang.RuntimeException: boom\n\tat com.example.app.Main.run(Main.java:1)\n"
	}
	out := "Drop box contents: 2 entries\n" + entry("2026-10-07 10:00:00") + "\n" + entry("2026-10-10 09:00:00")
	newest := time.Date(2026, 10, 10, 12, 0, 0, 0, loc).UnixNano()
	for _, tc := range []struct {
		maxAge time.Duration
		want   int
	}{{24 * time.Hour, 1}, {7 * 24 * time.Hour, 2}} {
		st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: tc.maxAge, IdleRelease: -1})
		if err != nil {
			t.Fatal(err)
		}
		st.Append([]*model.Record{{TimeNs: newest, Tag: "T", Msg: "m", Level: 'I'}})
		st.Flush()
		p := NewPipeline(st, "b1", loc)
		p.ApplyDropbox(out)
		p.ApplyDropbox(out) // idempotent
		if n := len(LoadCrashes(st)); n != tc.want {
			t.Errorf("MaxAge %v: %d crashes imported, want %d", tc.maxAge, n, tc.want)
		}
		st.Close()
	}
}

// Aux retention runs on host time (#48, #50): a device clock reset to 1970
// must not drop fresh entries, and one set a year ahead must not keep old
// ones. Lines written before host times were stored keep the old rule.
func TestCompactAuxUsesHostTime(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	epoch := int64(3605e9)                                   // device clock 1970-01-01 01:00:05
	ahead := time.Now().Add(365 * 24 * time.Hour).UnixNano() // device clock a year ahead
	p.EmitCrash(&analysis.Crash{Kind: "java", Boot: "b1", TimeNs: epoch, PID: 10, Headline: "fresh crash on a 1970 device clock"})
	p.EmitCrash(&analysis.Crash{Kind: "java", Boot: "b1", TimeNs: ahead, PID: 11, Headline: "crash a year ahead"})
	appendProc(st, procs.Proc{ID: "p1", Boot: "b1", PID: 10, Name: "com.a", StartNs: epoch - 1e9, EndNs: epoch, Source: "am_proc_start"})
	st.AppendAux("dropbox_seen", seenLine{Key: "data_app_crash|" + fmt.Sprint(epoch), HostNs: time.Now().UnixNano()})
	// legacy lines (no host time): device-time rule as before
	st.AppendAux("crashes", &analysis.Crash{ID: "cr99", Kind: "java", Boot: "b1", TimeNs: epoch, PID: 12, Headline: "legacy"})
	st.AppendAux("dropbox_seen", json.RawMessage(fmt.Sprintf("%q", "data_app_anr|"+fmt.Sprint(epoch))))

	CompactAux(st, time.Now())
	var heads []string
	for _, c := range LoadCrashes(st) {
		heads = append(heads, c.Headline)
	}
	if len(heads) != 2 {
		t.Fatalf("after compaction now: crashes %v, want the 1970 and the year-ahead crash (legacy one dropped)", heads)
	}
	if n := len(LoadProcs(st)); n != 1 {
		t.Fatalf("ended 1970 process dropped: %d procs", n)
	}
	seen := st.ReadAux("dropbox_seen")
	if len(seen) != 1 {
		t.Fatalf("dropbox_seen %q, want only the host-stamped key", seen)
	}
	if k, _, ok := parseSeen(seen[0]); !ok || k != "data_app_crash|"+fmt.Sprint(epoch) {
		t.Fatalf("dropbox_seen key %q", seen[0])
	}
	// a day and a bit later in host time everything ages out, also the
	// crash dated a year ahead
	CompactAux(st, time.Now().Add(25*time.Hour))
	if c := LoadCrashes(st); len(c) != 0 {
		t.Fatalf("crashes kept by device time: %d", len(c))
	}
	if n := len(LoadProcs(st)); n != 0 {
		t.Fatalf("ended procs kept: %d", n)
	}
	if n := len(st.ReadAux("dropbox_seen")); n != 0 {
		t.Fatalf("dropbox_seen kept: %d", n)
	}
}

// ApplyDropbox reads dropbox_seen in both formats: a key seen before the
// upgrade (bare JSON string) is not imported again.
func TestDropboxSeenBothFormats(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		k, h, ok := parseSeen([]byte(map[bool]string{true: `"data_app_crash|123"`, false: `{"k":"data_app_crash|123","ht":5}`}[legacy]))
		if !ok || k != "data_app_crash|123" || (h != 0) != !legacy {
			t.Fatalf("legacy=%v: %q %d %v", legacy, k, h, ok)
		}
	}
}
