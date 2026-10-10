package store

import (
	"os"
	"strconv"
	"time"
)

// EnvInt64 reads a positive integer environment variable (ok=false if unset
// or invalid).
func EnvInt64(name string) (int64, bool) {
	v, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	return v, err == nil && v >= 0
}

// ConfigFromEnv builds a Config from ANDROID_LOG_MCP_* variables; unset
// values keep their defaults.
//
//	ANDROID_LOG_MCP_MEM_RECORDS       recent window per device, records (20000)
//	ANDROID_LOG_MCP_MEM_MB            recent window per device, MiB (16)
//	ANDROID_LOG_MCP_HISTORY_CACHE_MB  decoded history cache per device, MiB (32)
//	ANDROID_LOG_MCP_IDLE_RELEASE      release caches after this long without queries (2m; 0 = never)
//	ANDROID_LOG_MCP_MAX_DEVICE_MB     stored logs per device, MiB (512)
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
	if s := os.Getenv("ANDROID_LOG_MCP_IDLE_RELEASE"); s != "" {
		if s == "0" {
			c.IdleRelease = -1
		} else if d, err := time.ParseDuration(s); err == nil {
			if d <= 0 {
				d = -1
			}
			c.IdleRelease = d
		}
	}
	if v, ok := EnvInt64("ANDROID_LOG_MCP_MAX_DEVICE_MB"); ok && v > 0 {
		c.MaxBytes = v << 20
	}
	return c
}
