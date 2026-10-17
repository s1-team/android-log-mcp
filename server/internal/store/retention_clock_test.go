package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Issues #184, #186, #189: segment age for rotation and retention comes
// from host-time stamps kept in meta.json, clamped to now, not from file
// mtimes.

func setHost(t *testing.T, at time.Time) {
	t.Helper()
	old := hostNow
	hostNow = func() time.Time { return at }
	t.Cleanup(func() { hostNow = old })
}

// segments written at host time `at`, each file stamped like the clock then.
func writeSegments(t *testing.T, base string, cfg Config, at time.Time, n int) string {
	t.Helper()
	setHost(t, at)
	s, err := Open(base, "x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		s.Append(mk(20, int64(i)*1e9))
	}
	s.Close()
	for _, sg := range ListSegments(s.Dir) {
		os.Chtimes(sg.Path, at, at)
	}
	return s.Dir
}

func nSegs(dir string) int { return len(ListSegments(dir)) }

// #184: the host clock was far off while recording and was corrected: one
// Prune (and OpenDir's prune) must not delete the recent data; the
// segments still age out one MaxAge after the correction.
func TestRetentionSurvivesHostClockCorrection(t *testing.T) {
	base := t.TempDir()
	cfg := Config{SegmentBytes: 3000, MaxAge: time.Hour}
	dir := writeSegments(t, base, cfg, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	before := nSegs(dir)
	if before < 4 {
		t.Fatalf("setup: %d segments", before)
	}
	now := time.Now()
	setHost(t, now)
	s, err := Open(base, "x", cfg) // OpenDir prunes
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := nSegs(dir); got != before {
		t.Fatalf("%d of %d segments left after OpenDir on a corrected clock", got, before)
	}
	s.Prune()
	if got := nSegs(dir); got != before {
		t.Fatalf("%d of %d segments left after Prune on a corrected clock", got, before)
	}
	setHost(t, now.Add(2*time.Hour)) // a MaxAge after the correction they age out
	s.Prune()
	if got := nSegs(dir); got != 1 {
		t.Fatalf("%d segments after MaxAge passed, want 1 (the active one)", got)
	}
}

// #186: restarts more often than MaxAge/4 do not restart the segment's age.
func TestRestartsDoNotDefeatAgeRetention(t *testing.T) {
	base := t.TempDir()
	cfg := Config{MaxAge: 24 * time.Hour}
	realNow := time.Now()
	start := realNow.Add(-50 * time.Hour)
	var dir string
	for i := 0; i < 10; i++ {
		fake := start.Add(time.Duration(i) * 5 * time.Hour)
		setHost(t, fake)
		s, err := Open(base, "d", cfg)
		if err != nil {
			t.Fatal(err)
		}
		dir = s.Dir
		s.Append(mk(1, fake.UnixNano()))
		s.Close()
		for _, sg := range ListSegments(dir) {
			os.Chtimes(sg.Path, fake, fake) // what a real last write does
		}
	}
	setHost(t, realNow)
	s, _ := Open(base, "d", cfg)
	defer s.Close()
	age := realNow.Sub(time.Unix(0, s.oldestNs()))
	if age > 31*time.Hour {
		t.Fatalf("oldest retained record is %v old (segments=%d), exceeds 1.25 x MaxAge", age.Round(time.Hour), nSegs(dir))
	}
}

// #189: a segment started "in the future" (the host clock was ahead and was
// corrected) still rotates and still ages out.
func TestFutureSegmentStartRotatesAndAgesOut(t *testing.T) {
	base := t.TempDir()
	cfg := Config{SegmentBytes: 1 << 20, MaxAge: 4 * time.Hour}
	now := time.Now()
	dir := writeSegments(t, base, cfg, now.Add(48*time.Hour), 3) // one segment, started 48 h ahead
	if nSegs(dir) != 1 {
		t.Fatalf("setup: %d segments", nSegs(dir))
	}
	setHost(t, now)
	s, err := Open(base, "x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Append(mk(1, 100e9))
	if nSegs(dir) != 1 {
		t.Fatalf("rotated right after the correction")
	}
	setHost(t, now.Add(cfg.MaxAge/4+time.Minute)) // a quarter of MaxAge after the correction
	s.Append(mk(1, 101e9))
	if got := nSegs(dir); got != 2 {
		t.Fatalf("%d segments, want 2: the future-dated segment never rotates", got)
	}
	setHost(t, now.Add(cfg.MaxAge+time.Hour))
	s.Prune()
	if got := nSegs(dir); got != 1 {
		t.Fatalf("%d segments, want 1: the future-dated segment never ages out", got)
	}
}

// A store written before the stamps existed (no seg_times in meta.json):
// opens, gets stamps from the file mtimes, and keeps working.
func TestRetentionStoreWithoutSegTimes(t *testing.T) {
	base := t.TempDir()
	cfg := Config{SegmentBytes: 3000, MaxAge: time.Hour}
	now := time.Now()
	dir := writeSegments(t, base, cfg, now.Add(-30*time.Minute), 10)
	mp := filepath.Join(dir, "meta.json")
	var m Meta
	b, _ := os.ReadFile(mp)
	if err := jsonUnmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.SegTimes) == 0 {
		t.Fatal("setup: no seg_times written")
	}
	m.SegTimes = nil
	nb, _ := jsonMarshal(&m)
	os.WriteFile(mp, nb, 0o644)
	before := nSegs(dir)
	setHost(t, now)
	s, err := Open(base, "x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := nSegs(dir); got != before {
		t.Fatalf("%d of %d segments after opening an old-format store (mtimes are 30 min old, MaxAge 1 h)", got, before)
	}
	if n := len(s.Meta().SegTimes); n != before {
		t.Fatalf("%d stamps, want %d (filled from the mtimes)", n, before)
	}
	setHost(t, now.Add(2*time.Hour))
	s.Prune()
	if got := nSegs(dir); got != 1 {
		t.Fatalf("%d segments after MaxAge, want 1", got)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
