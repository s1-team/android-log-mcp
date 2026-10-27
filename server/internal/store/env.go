package store

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// SkipSegCRC64Env is the opt-in to skip full-segment CRC64 verify when loading
// seg-*.idx sidecars (#271). Default off: verify. Payload CRC and header
// identity checks (magic, version, first_seq, size) still run.
const SkipSegCRC64Env = "ANDROID_LOG_MCP_SKIP_SEG_CRC64"

// EnvOn reports whether v turns an opt-in flag on (1 / true / yes / on).
func EnvOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// EnvInt64 reads a non-negative integer environment variable (ok=false if
// unset or invalid; invalid values are reported on stderr).
func EnvInt64(name string) (int64, bool) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		log.Printf("ignoring %s=%q: not a non-negative integer; using the default", name, s)
		return 0, false
	}
	return v, true
}

// ParseRetention parses a retention period: a Go duration ("72h", "90m")
// or a number of days ("1d", "7d", "1.5d"). It must be positive.
func ParseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if num, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = time.Duration(f * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

// FormatRetention renders a retention period ("1d", "36h", "90m").
func FormatRetention(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}

// ConfigFromEnv builds a Config from ANDROID_LOG_MCP_* variables; unset or
// invalid values keep their defaults (invalid ones with a warning on stderr).
//
//	ANDROID_LOG_MCP_MEM_RECORDS       recent window per device, records (20000)
//	ANDROID_LOG_MCP_MEM_MB            recent window per device, MiB (16)
//	ANDROID_LOG_MCP_HISTORY_CACHE_MB  decoded history cache per device, MiB (32)
//	ANDROID_LOG_MCP_IDLE_RELEASE      release caches after this long without queries (2m; 0 = never)
//	ANDROID_LOG_MCP_MAX_DEVICE_MB     stored logs per device, MiB (512)
//	ANDROID_LOG_MCP_MAX_AGE           retention: Go duration or days, e.g. 72h, 7d, 1.5d (1d)
//	ANDROID_LOG_MCP_SKIP_SEG_CRC64    skip seg_crc64 verify on idx load (off; 1/true/yes/on)
func ConfigFromEnv() Config {
	var c Config
	if v, ok := EnvInt64("ANDROID_LOG_MCP_MEM_RECORDS"); ok && v > 0 {
		c.MemRecords = int(v)
	}
	if v, ok := EnvInt64("ANDROID_LOG_MCP_MEM_MB"); ok && v > 0 {
		c.MemBytes = v << 20
	}
	if v, ok := EnvInt64("ANDROID_LOG_MCP_HISTORY_CACHE_MB"); ok && v > 0 {
		c.HistoryCacheBytes = v << 20
	}
	if s := strings.TrimSpace(os.Getenv("ANDROID_LOG_MCP_IDLE_RELEASE")); s != "" {
		if s == "0" {
			c.IdleRelease = -1
		} else if d, err := time.ParseDuration(s); err == nil {
			if d <= 0 {
				d = -1
			}
			c.IdleRelease = d
		} else {
			log.Printf("ignoring ANDROID_LOG_MCP_IDLE_RELEASE=%q: not a duration like 2m; using the default", s)
		}
	}
	if v, ok := EnvInt64("ANDROID_LOG_MCP_MAX_DEVICE_MB"); ok && v > 0 {
		c.MaxBytes = v << 20
	}
	if s := os.Getenv("ANDROID_LOG_MCP_MAX_AGE"); strings.TrimSpace(s) != "" {
		if d, err := ParseRetention(s); err == nil {
			c.MaxAge = d
		} else {
			log.Printf("ignoring ANDROID_LOG_MCP_MAX_AGE=%q: %v (use e.g. 72h or 7d); using the default %s", s, err, FormatRetention(DefaultMaxAge))
		}
	}
	if EnvOn(os.Getenv(SkipSegCRC64Env)) {
		c.SkipSegCRC64 = true
	}
	return c
}
