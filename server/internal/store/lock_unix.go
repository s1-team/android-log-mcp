//go:build unix

package store

import (
	"os"
	"syscall"
	"time"
)

// lockRetry bounds how long tryLock retries a lock that looks held (issue
// #82). flock belongs to the open file description, which is released only
// when its last fd is closed. A child process being started (fork, before
// exec) holds a copy of every open fd, O_CLOEXEC ones included, so a lock
// the parent has just released can still look held for a few milliseconds.
// The longest wait measured was 22.6 ms (macOS CI runner); 200 ms leaves a
// margin of about 9. A lock that really is held is reported only after it.
// Tests set it to 0 to see the failure without the retry.
var lockRetry = 200 * time.Millisecond

// tryLock takes an exclusive flock without blocking. While the lock looks
// held it retries with a 1 ms to 10 ms backoff for up to lockRetry, keeping
// the file open (flock on the same fd is idempotent), then reports it held.
// Any other error fails at once. The file stays open while held.
func tryLock(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(lockRetry)
	wait := time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, true
		}
		if err != syscall.EWOULDBLOCK || !time.Now().Add(wait).Before(deadline) {
			f.Close()
			return nil, false
		}
		time.Sleep(wait)
		if wait < 10*time.Millisecond {
			wait *= 2
		}
		if wait > 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}
	}
}

// refreshLock: not needed, flock never goes stale.
func refreshLock(string) {}
