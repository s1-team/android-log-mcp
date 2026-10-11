package fakeadb

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// TestHelperSleep is the child process of the Alive tests.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("ALM_FAKEADB_ALIVE_HELPER") == "" {
		t.Skip("helper process only")
	}
	time.Sleep(time.Minute)
}

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := procattr.Hide(exec.Command(os.Args[0], "-test.run=^TestHelperSleep$"))
	cmd.Env = append(os.Environ(), "ALM_FAKEADB_ALIVE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// A running process is alive and not exiting; once it is killed it is not
// alive even before it is reaped (#61: a zombie has exited), and after
// Wait it is gone.
func TestAlive(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	if !Alive(pid) || Exiting(pid) {
		t.Fatalf("running child: Alive=%v Exiting=%v", Alive(pid), Exiting(pid))
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		// not reaped yet (no Wait): a zombie must not count as alive
		end := time.Now().Add(10 * time.Second)
		for Alive(pid) && time.Now().Before(end) {
			time.Sleep(5 * time.Millisecond)
		}
		if Alive(pid) {
			t.Fatal("killed, unreaped child still alive after 10s")
		}
		if !zombie(pid) {
			t.Fatal("child was reaped before Wait; the zombie case was not tested")
		}
		if Exiting(pid) {
			t.Fatal("a zombie is not exiting")
		}
	}
	cmd.Wait()
	if Alive(pid) || Exiting(pid) {
		t.Fatalf("reaped child: Alive=%v Exiting=%v", Alive(pid), Exiting(pid))
	}
}
