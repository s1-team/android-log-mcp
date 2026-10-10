package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Runs on every CI OS (Windows: LockFileEx; unix: flock).
func TestLockExclusiveAndReleaseOnClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	f, ok := TryLock(p)
	if !ok {
		t.Fatal("first lock failed")
	}
	if _, ok := TryLock(p); ok {
		t.Fatal("second lock succeeded while held")
	}
	if !LockHeld(p) {
		t.Fatal("LockHeld false while held")
	}
	// the content stays readable by others while locked (owner info)
	if _, err := f.WriteAt([]byte(`{"pid":1}`), 0); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != `{"pid":1}` {
		t.Fatalf("locked file not readable: %q %v", b, err)
	}
	f.Close() // closing the handle releases the lock (no ReleaseLock)
	g, ok := TryLock(p)
	if !ok {
		t.Fatal("lock not released by closing the file")
	}
	ReleaseLock(g)
	if LockHeld(p) {
		t.Fatal("LockHeld after release")
	}
}

// A process that dies while holding the lock releases it (no staleness wait).
func TestLockReleasedOnProcessExit(t *testing.T) {
	if p := os.Getenv("ALM_LOCK_CHILD"); p != "" {
		if _, ok := TryLock(p); !ok {
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Hour) // killed by the parent
		return
	}
	p := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedOnProcessExit$")
	cmd.Env = append(os.Environ(), "ALM_LOCK_CHILD="+p)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := out.Read(buf); err != nil || string(buf[:6]) != "locked" {
		cmd.Process.Kill()
		t.Fatalf("child did not lock: %q %v", buf, err)
	}
	if _, ok := TryLock(p); ok {
		cmd.Process.Kill()
		t.Fatal("lock not exclusive across processes")
	}
	cmd.Process.Kill()
	cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f, ok := TryLock(p); ok {
			ReleaseLock(f)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lock still held after the holder was killed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
