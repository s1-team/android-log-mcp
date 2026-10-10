//go:build !unix

package store

import "os"

// tryLock on non-unix platforms: best effort exclusive create. A stale lock
// left by a crashed process is ignored after it is older than 1 minute and not
// refreshed (the writer touches it periodically).
func tryLock(path string) (*os.File, bool) {
	if st, err := os.Stat(path); err == nil && timeSince(st.ModTime()) > 60 {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	return f, true
}
