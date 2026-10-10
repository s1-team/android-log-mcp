//go:build !unix && !windows

package store

import (
	"os"
	"time"
)

// tryLock on platforms with neither flock nor LockFileEx: best effort
// exclusive create. A lock left by a crashed process is ignored once it is
// older than 1 minute and not refreshed (holders call refreshLock).
func tryLock(path string) (*os.File, bool) {
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) > time.Minute {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	return f, true
}

// refreshLock keeps a held fallback lock from looking stale.
func refreshLock(path string) {
	now := time.Now()
	os.Chtimes(path, now, now)
}
