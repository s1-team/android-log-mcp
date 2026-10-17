//go:build windows

package device

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// Helper: a child that just sleeps (started by TestKillPidWaitsForExit).
func TestKillPidHelperSleep(t *testing.T) {
	if os.Getenv("ALM_KILLPID_HELPER") != "1" {
		t.Skip("helper child only")
	}
	time.Sleep(time.Minute)
}

// killPid returns only after the process has exited: its working directory
// can be removed right away (issue #73; a still-exiting process locks it).
func TestKillPidWaitsForExit(t *testing.T) {
	dir := t.TempDir()
	cmd := procattr.Hide(exec.Command(os.Args[0], "-test.run=^TestKillPidHelperSleep$"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ALM_KILLPID_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	time.Sleep(300 * time.Millisecond) // let it start and take its cwd
	if !killPidWait(cmd.Process.Pid, 5*time.Second) {
		t.Fatal("killPidWait did not see the process exit")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("working dir still locked right after killPid: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child not reaped")
	}
}

// A zero wait never blocks: killPidWait returns at once (the injected wait
// is what the tests of slow-exit handling use, no 500 ms sleep needed).
func TestKillPidWaitZeroDoesNotBlock(t *testing.T) {
	cmd := procattr.Hide(exec.Command(os.Args[0], "-test.run=^TestKillPidHelperSleep$"))
	cmd.Env = append(os.Environ(), "ALM_KILLPID_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t0 := time.Now()
	killPidWait(cmd.Process.Pid, 0)
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("killPidWait(0) blocked for %s", time.Since(t0))
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child not reaped")
	}
}
