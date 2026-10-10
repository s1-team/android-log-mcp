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
	f, _ := store.Open(filepath.Dir(st.Dir), "d", store.Config{})
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
