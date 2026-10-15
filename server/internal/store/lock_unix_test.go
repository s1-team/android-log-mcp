//go:build unix

package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Issue #82: a child that inherited the lock's open file description keeps
// the flock held after the parent closed its fd, until the child exits (or
// execs, for an O_CLOEXEC fd). Here the child inherits it on purpose
// (ExtraFiles) and exits after 50 ms.
func TestTryLockWaitsForInheritedDescriptor(t *testing.T) {
	if os.Getenv("ALM_LOCK_SLEEP_CHILD") != "" {
		time.Sleep(50 * time.Millisecond)
		return
	}
	p := filepath.Join(t.TempDir(), "x.lock")
	hold := func() {
		f, ok := TryLock(p)
		if !ok {
			t.Fatal("first lock failed")
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestTryLockWaitsForInheritedDescriptor$")
		cmd.Env = append(os.Environ(), "ALM_LOCK_SLEEP_CHILD=1")
		cmd.ExtraFiles = []*os.File{f}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Wait() })
		f.Close() // the parent releases; the child's copy keeps the flock
	}
	// without the retry the just-released lock looks held
	old := lockRetry
	lockRetry = 0
	hold()
	if g, ok := TryLock(p); ok {
		ReleaseLock(g)
		t.Fatal("lock free while a child still holds a copy of the descriptor: the test does not reproduce #82")
	}
	lockRetry = old
	time.Sleep(200 * time.Millisecond) // that child is gone
	hold()
	t0 := time.Now()
	g, ok := TryLock(p)
	d := time.Since(t0)
	if !ok {
		t.Fatalf("lock still held after %v", d)
	}
	ReleaseLock(g)
	if d < 20*time.Millisecond || d > 190*time.Millisecond {
		t.Errorf("waited %v, want about 50 ms (under 200 ms)", d)
	}
}

// A lock that really is held is reported held, but only after the retry
// budget, and is taken once its holder is gone.
func TestTryLockHeldReportedAfterRetryBudget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	f, ok := TryLock(p)
	if !ok {
		t.Fatal("first lock failed")
	}
	t0 := time.Now()
	if _, ok := TryLock(p); ok {
		t.Fatal("second lock succeeded while held")
	}
	if d := time.Since(t0); d < lockRetry-20*time.Millisecond || d > lockRetry+300*time.Millisecond {
		t.Errorf("reported held after %v, want about %v", d, lockRetry)
	}
	ReleaseLock(f)
	if g, ok := TryLock(p); !ok {
		t.Fatal("not taken after the holder released it")
	} else {
		ReleaseLock(g)
	}
}
