package store

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func mkMsg(n int, t0 int64, msg func(i int) string) []*model.Record {
	out := mk(n, t0)
	for i, r := range out {
		r.Msg = msg(i)
	}
	return out
}

func TestRecentWindowRecordAndByteCaps(t *testing.T) {
	base := tempDir(t)
	cfg := Config{MemRecords: 100, MemBytes: 1 << 30, IdleRelease: -1}
	s, err := Open(base, "d", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for i := 0; i < 20; i++ {
		s.Append(mk(50, int64(i)*1e9))
	}
	if st := s.Stats(); st.MemRecords > 125 || st.MemRecords < 100 {
		t.Fatalf("record cap: window %d", st.MemRecords)
	}
	// long lines: the byte cap binds long before the record cap
	s2, _ := Open(base, "long", Config{MemRecords: 100_000, MemBytes: 200 << 10, IdleRelease: -1})
	t.Cleanup(s2.Close)
	big := strings.Repeat("x", 10<<10)
	for i := 0; i < 20; i++ {
		s2.Append(mkMsg(10, int64(i)*1e9, func(int) string { return big }))
	}
	st := s2.Stats()
	if st.MemBytes > 250<<10 || st.MemRecords > 15 || st.MemRecords == 0 {
		t.Fatalf("byte cap: window %d records, %d bytes", st.MemRecords, st.MemBytes)
	}
	if st.MemBytes != sumBytes(s2.mem) {
		t.Fatalf("byte accounting drifted: %d vs %d", st.MemBytes, sumBytes(s2.mem))
	}
	// everything is still readable (older lines come from disk)
	if got := collect(s2, 0, 0, false, 1000); len(got) != 200 || got[0] != 1 {
		t.Fatalf("scan after trim: %d", len(got))
	}
	// a reopened writer loads at most the caps, exactly
	s2.Close()
	s3, _ := Open(base, "long", Config{MemRecords: 100_000, MemBytes: 200 << 10, IdleRelease: -1})
	t.Cleanup(s3.Close)
	if st := s3.Stats(); st.MemBytes > 200<<10 || st.MemRecords == 0 || !st.MemLoaded {
		t.Fatalf("reopen window: %+v", st)
	}
}

func TestHistoryCacheByteCapLRU(t *testing.T) {
	base := tempDir(t)
	cfg := Config{SegmentBytes: 64 << 10, MemRecords: 10, BlockBytes: 4 << 10, HistoryCacheBytes: 48 << 10, IdleRelease: -1}
	s, _ := Open(base, "d", cfg)
	t.Cleanup(s.Close)
	for i := 0; i < 10; i++ {
		s.Append(mk(500, int64(i)*1e12))
	}
	if got := collect(s, 0, 0, true, 1<<30); len(got) != 5000 {
		t.Fatalf("full scan %d", len(got))
	}
	if c := s.HistoryCacheBytes(); c > 48<<10 || c == 0 {
		t.Fatalf("history cache %d bytes, cap %d", c, 48<<10)
	}
	var sum int64
	for e := s.lru.Front(); e != nil; e = e.Next() {
		sum += e.Value.(*cachedBlock).bytes
	}
	if sum != s.HistoryCacheBytes() || len(s.blocks) != s.lru.Len() {
		t.Fatalf("lru accounting: %d vs %d, %d vs %d", sum, s.HistoryCacheBytes(), len(s.blocks), s.lru.Len())
	}
	// LRU: re-reading the oldest records keeps them cached (front)
	collect(s, 0, 50, false, 1<<30)
	front := s.lru.Front().Value.(*cachedBlock)
	if front.recs[0].Seq != 1 {
		t.Fatalf("most recently used block starts at %d", front.recs[0].Seq)
	}
	// a block bigger than the whole cache is served but not cached
	s2, _ := Open(base, "big", Config{MemRecords: 1, BlockBytes: 1 << 20, HistoryCacheBytes: 1 << 10, IdleRelease: -1})
	t.Cleanup(s2.Close)
	s2.Append(mk(200, 1))
	if got := collect(s2, 0, 0, false, 1000); len(got) != 200 || s2.HistoryCacheBytes() != 0 {
		t.Fatalf("oversized block: %d records, cache %d", len(got), s2.HistoryCacheBytes())
	}
}

// History reads that fall back to disk (tiny window, tiny blocks, tiny
// cache, many segments) must return exactly what was appended.
func TestHistoryFallbackMatchesAppended(t *testing.T) {
	base := tempDir(t)
	cfg := Config{SegmentBytes: 16 << 10, MemRecords: 37, BlockBytes: 2 << 10, HistoryCacheBytes: 8 << 10, IdleRelease: -1}
	w, _ := Open(base, "d", cfg)
	t.Cleanup(w.Close)
	var all []*model.Record
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		b := mkMsg(1+rng.Intn(80), int64(i)*1e12, func(j int) string { return fmt.Sprintf("m%d-%d %s", i, j, strings.Repeat("y", rng.Intn(300))) })
		w.Append(b)
		all = append(all, b...)
	}
	f, _ := Open(base, "d", cfg) // follower: same data via a different path
	t.Cleanup(f.Close)
	f.Refresh()
	n := uint64(len(all))
	for _, s := range []*Store{w, f} {
		for k := 0; k < 200; k++ {
			lo, hi := uint64(rng.Intn(int(n)+1)), uint64(rng.Intn(int(n)+2))
			desc := k%2 == 1
			var want []string
			for _, r := range all {
				if r.Seq > lo && (hi == 0 || r.Seq < hi) {
					want = append(want, r.Msg)
				}
			}
			if desc {
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
			}
			var got []string
			s.Scan(lo, hi, desc, func(r *model.Record) bool { got = append(got, r.Msg); return true })
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("writer=%v scan(%d,%d,desc=%v): got %d records, want %d", s.IsWriter(), lo, hi, desc, len(got), len(want))
			}
		}
		if r := s.Get(1); r == nil || r.Msg != all[0].Msg {
			t.Fatal("get oldest")
		}
	}
}

func TestFollowerLoadsOnDemandAndReleasesWhenIdle(t *testing.T) {
	base := tempDir(t)
	cfg := Config{SegmentBytes: 32 << 10, MemRecords: 100, BlockBytes: 4 << 10, IdleRelease: 150 * time.Millisecond}
	w, _ := Open(base, "d", cfg)
	t.Cleanup(w.Close)
	w.Append(mk(1000, 0))
	f, _ := Open(base, "d", cfg)
	t.Cleanup(f.Close)
	st := f.Stats()
	if f.IsWriter() || st.MemLoaded || st.MemRecords != 0 || st.LastSeq != 1000 || st.NewestNs != 999e6 {
		t.Fatalf("follower should start unloaded with the tail known: %+v", st)
	}
	if got := collect(f, 0, 0, true, 3); fmt.Sprint(got) != "[1000 999 998]" {
		t.Fatalf("first query %v", got)
	}
	if st := f.Stats(); !st.MemLoaded || st.MemRecords != 100 {
		t.Fatalf("after first query: %+v", st)
	}
	collect(f, 0, 300, false, 1<<30) // history read → cache
	if f.HistoryCacheBytes() == 0 {
		t.Fatal("history cache unused")
	}
	// idle → released (window and history cache), writer keeps its window
	collect(w, 0, 300, false, 1<<30)
	time.Sleep(600 * time.Millisecond)
	if st := f.Stats(); st.MemLoaded || st.MemRecords != 0 || st.CacheBytes != 0 {
		t.Fatalf("follower not released: %+v", st)
	}
	if st := w.Stats(); !st.MemLoaded || st.MemRecords == 0 || st.CacheBytes != 0 {
		t.Fatalf("writer after idle: %+v", st)
	}
	// new data while released; the next query reloads and sees it
	w.Append(mk(5, 5e9))
	f.Refresh()
	if f.Stats().MemLoaded {
		t.Fatal("refresh must not load the window")
	}
	if got := collect(f, 0, 0, true, 2); fmt.Sprint(got) != "[1005 1004]" {
		t.Fatalf("after reload %v", got)
	}
	if got := collect(f, 0, 3, false, 10); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("history after reload %v", got)
	}
	// explicit release and promotion
	f.ReleaseCaches()
	w.Close()
	if !f.TryPromote() {
		t.Fatal("promote")
	}
	f.Append(mk(1, 9e9))
	if got := collect(f, 0, 0, true, 3); fmt.Sprint(got) != "[1006 1005 1004]" {
		t.Fatalf("after promote %v", got)
	}
}

func TestDiskGuardHysteresis(t *testing.T) {
	free := uint64(10 << 30)
	var fail bool
	g := &DiskGuard{MinFree: 512 << 20, Free: func(string) (uint64, error) {
		if fail {
			return 0, errors.New("statfs failed")
		}
		return free, nil
	}}
	step := func(f uint64, wantPaused, wantChanged bool) {
		t.Helper()
		free = f
		p, c := g.Check("/x")
		if p != wantPaused || c != wantChanged || g.Paused() != wantPaused {
			t.Fatalf("free %d MiB: paused=%v changed=%v", f>>20, p, c)
		}
	}
	step(10<<30, false, false)
	step(600<<20, false, false)
	step(500<<20, true, true) // below 512 MiB → pause
	if r := g.Reason(); !strings.Contains(r, "500 MiB free") || !strings.Contains(r, "1024 MiB") {
		t.Fatalf("reason %q", r)
	}
	step(700<<20, true, false) // above MinFree but below 2× → still paused
	fail = true
	step(0, true, false) // unknown → unchanged
	fail = false
	step(1024<<20, false, true) // recovered
	if g.Reason() != "" {
		t.Fatal("reason while running")
	}
	if p, _ := (&DiskGuard{}).Check("/x"); p {
		t.Fatal("MinFree 0 disables")
	}
	var nilGuard *DiskGuard
	if nilGuard.Paused() || nilGuard.Reason() != "" {
		t.Fatal("nil guard")
	}
	if _, err := FreeSpace(t.TempDir()); err != nil && !errors.Is(err, errNoFreeSpace) {
		t.Fatalf("platform free space: %v", err)
	}
}

func TestEnforceTotalDirectoryCap(t *testing.T) {
	base := tempDir(t)
	cfg := Config{SegmentBytes: 4 << 10, IdleRelease: -1}
	// "old": recorded earlier, nobody writes it now
	o, _ := Open(base, "old", cfg)
	o.Append(mk(200, 0))
	o.Close()
	// "own": recorded by this process
	w, _ := Open(base, "own", cfg)
	t.Cleanup(w.Close)
	w.Append(mk(200, 0))
	// "busy": recorded by another process (simulated by another writer store)
	b, _ := Open(base, "busy", cfg)
	t.Cleanup(b.Close)
	b.Append(mk(200, 0))
	// age: old < own < busy, segment by segment
	age := func(dir string, start time.Time) {
		st := &Store{Dir: filepath.Join(base, dir)}
		segs, _ := st.listSegments()
		for i, sg := range segs {
			tm := start.Add(time.Duration(i) * time.Minute)
			os.Chtimes(sg.path, tm, tm)
		}
	}
	now := time.Now()
	age("old", now.Add(-3*time.Hour))
	age("own", now.Add(-2*time.Hour))
	age("busy", now.Add(-4*time.Hour)) // oldest of all, but not ours to delete
	before := DirBytes(base)
	limit := before * 2 / 3
	freed := EnforceTotal(base, limit, map[string]*Store{w.Dir: w})
	after := DirBytes(base)
	if freed == 0 || after != before-freed {
		t.Fatalf("freed %d, before %d after %d", freed, before, after)
	}
	if after > limit {
		t.Fatalf("still over the cap: %d > %d", after, limit)
	}
	segsOf := func(dir string) []segment {
		st := &Store{Dir: filepath.Join(base, dir)}
		s, _ := st.listSegments()
		return s
	}
	if b.FirstSeq() != 1 || segsOf("busy")[0].first != 1 {
		t.Fatal("another process's directory was pruned")
	}
	if segsOf("old")[0].first == 1 {
		t.Fatal("the oldest unowned directory should be pruned first")
	}
	// the owned store stays consistent with what is on disk
	if w.FirstSeq() != max(segsOf("own")[0].first, 1) {
		t.Fatalf("owned first seq %d vs disk %d", w.FirstSeq(), segsOf("own")[0].first)
	}
	if got := collect(w, 0, 0, false, 1<<30); len(got) == 0 || got[0] != w.FirstSeq() || got[len(got)-1] != 200 {
		t.Fatalf("owned scan after prune: %v", got)
	}
	// an impossible cap removes everything but the newest segment of each device
	EnforceTotal(base, 1, map[string]*Store{w.Dir: w})
	if n := len(segsOf("old")); n != 1 {
		t.Fatalf("old keeps %d segments", n)
	}
	if n := len(segsOf("own")); n != 1 {
		t.Fatalf("own keeps %d segments", n)
	}
	if EnforceTotal(base, 0, nil) != 0 {
		t.Fatal("0 disables")
	}
}

func TestCompactAuxAndCleanTmp(t *testing.T) {
	base := tempDir(t)
	s, _ := Open(base, "d", Config{IdleRelease: -1})
	t.Cleanup(s.Close)
	for i := 0; i < 50; i++ {
		s.AppendAux("x", map[string]int{"k": i % 5, "v": i})
	}
	before, after, err := s.CompactAux("x", func(lines [][]byte) [][]byte { return lines[len(lines)-5:] })
	if err != nil || after >= before || len(s.ReadAux("x")) != 5 {
		t.Fatalf("compact %d→%d %v (%d lines)", before, after, err, len(s.ReadAux("x")))
	}
	// no gain → file untouched
	if b, a, _ := s.CompactAux("x", func(l [][]byte) [][]byte { return l }); a != b {
		t.Fatal("rewrote without gain")
	}
	s.AppendAux("x", 1) // appends keep working after a rewrite
	if len(s.ReadAux("x")) != 6 {
		t.Fatal("append after compaction")
	}
	if b, a, err := s.CompactAux("missing", nil); b != 0 || a != 0 || err != nil {
		t.Fatal("missing file")
	}
	oldTmp := filepath.Join(s.Dir, "meta.json.tmp")
	newTmp := filepath.Join(s.Dir, "procs.jsonl.tmp")
	os.WriteFile(oldTmp, []byte("x"), 0o644)
	os.WriteFile(newTmp, []byte("x"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(oldTmp, old, old)
	if n := s.CleanTmp(10 * time.Minute); n != 1 {
		t.Fatalf("cleaned %d", n)
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Fatal("a fresh tmp file (rewrite in progress) must stay")
	}
	f, _ := Open(base, "d", Config{})
	t.Cleanup(f.Close)
	if _, _, err := f.CompactAux("x", nil); err == nil || f.CleanTmp(0) != 0 {
		t.Fatal("followers must not compact")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("ANDROID_LOG_MCP_MEM_RECORDS", "15000")
	t.Setenv("ANDROID_LOG_MCP_MEM_MB", "8")
	t.Setenv("ANDROID_LOG_MCP_HISTORY_CACHE_MB", "16")
	t.Setenv("ANDROID_LOG_MCP_IDLE_RELEASE", "0")
	t.Setenv("ANDROID_LOG_MCP_MAX_DEVICE_MB", "256")
	c := ConfigFromEnv()
	if c.MemRecords != 15000 || c.MemBytes != 8<<20 || c.HistoryCacheBytes != 16<<20 || c.IdleRelease >= 0 || c.MaxBytes != 256<<20 {
		t.Fatalf("%+v", c)
	}
	t.Setenv("ANDROID_LOG_MCP_IDLE_RELEASE", "30s")
	t.Setenv("ANDROID_LOG_MCP_MEM_MB", "junk")
	c = ConfigFromEnv()
	c.defaults()
	if c.IdleRelease != 30*time.Second || c.MemBytes != DefaultMemBytes {
		t.Fatalf("%+v", c)
	}
	var d Config
	d.defaults()
	if d.MemRecords != 20_000 || d.HistoryCacheBytes != 32<<20 || d.IdleRelease != 2*time.Minute {
		t.Fatalf("defaults %+v", d)
	}
}
