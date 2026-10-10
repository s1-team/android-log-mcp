//go:build windows

package store

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockOffsetHigh places the locked byte at offset 2^62, far past the end of
// any lock file: Windows byte-range locks are mandatory, so locking the
// content would make the owner info (JSON at offset 0) unreadable to other
// processes. A lock past EOF is allowed and still excludes other lockers.
const lockOffsetHigh = 1 << 30

// tryLock takes an exclusive, non-blocking LockFileEx lock; it is held while
// the file stays open and the OS releases it when the handle is closed or
// the process dies (no staleness heuristics).
func tryLock(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	ol := &windows.Overlapped{OffsetHigh: lockOffsetHigh}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// refreshLock: not needed, the OS lock never goes stale.
func refreshLock(string) {}
