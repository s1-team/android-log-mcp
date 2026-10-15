package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Issue #82: re-taking a lock this process just released, while other
// goroutines start child processes, never fails (nobody else holds it).
// Light: about 1 s, 2 spawners. Windows has no such race; the test still
// has to pass there.
func TestRelockWhileStartingChildren(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				exec.Command(os.Args[0], "-test.run=^$").Run()
			}
		}()
	}
	fails, n := 0, 0
	for end := time.Now().Add(time.Second); time.Now().Before(end); n++ {
		f, ok := TryLock(p)
		if !ok {
			fails++
			continue
		}
		ReleaseLock(f)
	}
	close(stop)
	wg.Wait()
	if fails > 0 {
		t.Fatalf("%d of %d re-locks failed although no other process held the lock", fails, n)
	}
	t.Logf("%d re-locks, 0 failed", n)
}
