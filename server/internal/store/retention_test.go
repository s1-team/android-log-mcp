package store

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	ok := map[string]time.Duration{
		"72h": 72 * time.Hour, "90m": 90 * time.Minute, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
		"1.5d": 36 * time.Hour, " 2d ": 48 * time.Hour, "0.5d": 12 * time.Hour, "1h30m": 90 * time.Minute,
	}
	for in, want := range ok {
		if got, err := ParseRetention(in); err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ", "abc", "0", "0d", "-1d", "-5h", "d", "1w", "7days", "1.d5"} {
		if got, err := ParseRetention(in); err == nil {
			t.Errorf("%q: accepted as %v", in, got)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "1d", 7 * 24 * time.Hour: "7d", 36 * time.Hour: "36h", 90 * time.Minute: "1h30m0s"} {
		if got := FormatRetention(d); got != want {
			t.Errorf("format %v: %q", d, got)
		}
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestMaxAgeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
		warn bool
	}{
		{"", DefaultMaxAge, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"72h", 72 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"bogus", DefaultMaxAge, true},
		{"0", DefaultMaxAge, true},
		{"-2d", DefaultMaxAge, true},
	} {
		buf := captureLog(t)
		t.Setenv("ANDROID_LOG_MCP_MAX_AGE", tc.env)
		c := ConfigFromEnv()
		c.defaults()
		if c.MaxAge != tc.want {
			t.Errorf("%q: MaxAge %v, want %v", tc.env, c.MaxAge, tc.want)
		}
		warned := strings.Contains(buf.String(), "ignoring ANDROID_LOG_MCP_MAX_AGE")
		if warned != tc.warn {
			t.Errorf("%q: warning=%v (%q)", tc.env, warned, buf.String())
		}
	}
	var d Config
	d.defaults()
	if d.MaxAge != 24*time.Hour {
		t.Fatalf("default retention %v", d.MaxAge)
	}
	buf := captureLog(t)
	t.Setenv("ANDROID_LOG_MCP_MAX_AGE", "")
	t.Setenv("ANDROID_LOG_MCP_MEM_MB", "lots")
	ConfigFromEnv()
	if !strings.Contains(buf.String(), "ignoring ANDROID_LOG_MCP_MEM_MB") {
		t.Fatalf("other invalid env vars warn too: %q", buf.String())
	}
}

// Retention deletes whole segments whose newest line (mtime) is older than
// the configured MaxAge; the active segment is never deleted.
func TestRetentionHonorsConfiguredMaxAge(t *testing.T) {
	for _, tc := range []struct {
		maxAge time.Duration
		keep   int // old segments that survive
	}{{2 * time.Hour, 0}, {4 * time.Hour, 3}, {7 * 24 * time.Hour, 3}} {
		base := tempDir(t)
		s, _ := Open(base, "d", Config{SegmentBytes: 1000, MaxAge: tc.maxAge, IdleRelease: -1})
		now := time.Now().UnixNano()
		for i := 0; i < 4; i++ { // 4 segments, records "now" so no age rotation
			s.Append(mk(12, now))
		}
		segs, _ := s.listSegments()
		if len(segs) < 4 {
			t.Fatalf("setup: %d segments", len(segs))
		}
		old := time.Now().Add(-3 * time.Hour)
		for _, sg := range segs[:len(segs)-1] {
			os.Chtimes(sg.path, old, old)
		}
		s.Prune()
		after, _ := s.listSegments()
		if got := len(after) - 1; got != min(tc.keep, len(segs)-1) {
			t.Errorf("MaxAge %v: %d old segments left of %d", tc.maxAge, got, len(segs)-1)
		}
		if s.FirstSeq() != max(after[0].first, 1) {
			t.Errorf("first seq %d vs %d", s.FirstSeq(), after[0].first)
		}
		s.Close()
	}
}

// A quiet device never fills a segment: the active segment is rotated once
// it spans more than a quarter of MaxAge so retention can delete it.
func TestActiveSegmentRotatesByAge(t *testing.T) {
	base := tempDir(t)
	cfg := Config{MaxAge: 4 * time.Hour, IdleRelease: -1}
	s, _ := Open(base, "d", cfg)
	t0 := time.Now().Add(-3 * time.Hour).UnixNano()
	s.Append(mk(3, t0))
	s.Append(mk(3, t0+int64(30*time.Minute))) // within 1h: same segment
	if segs, _ := s.listSegments(); len(segs) != 1 {
		t.Fatalf("rotated too early: %d", len(segs))
	}
	s.Append(mk(3, t0+int64(2*time.Hour))) // > MaxAge/4 after the first line
	if segs, _ := s.listSegments(); len(segs) != 2 {
		t.Fatalf("no age rotation: %d segments", len(segs))
	}
	s.Close()
	// reopening does not append to an active segment that is already too old
	s2, _ := Open(base, "d", cfg)
	t.Cleanup(s2.Close)
	s2.Append(mk(1, time.Now().UnixNano()))
	if segs, _ := s2.listSegments(); len(segs) != 3 {
		t.Fatalf("reused an old segment after reopen: %d segments", len(segs))
	}
	if got := collect(s2, 0, 0, false, 100); len(got) != 10 || got[9] != 10 {
		t.Fatalf("records %v", got)
	}
}
