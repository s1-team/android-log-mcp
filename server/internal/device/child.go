package device

import (
	"context"
	"os/exec"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// adbCmd builds an adb child process of the recorder. It is tied to the
// recorder's lifetime and never opens a console window on Windows
// (procattr.Hide): cancelling ctx kills it (graceful stop), and on
// Linux it also gets SIGKILL if the recorder dies (Pdeathsig). Elsewhere an
// orphaned `adb logcat` exits on its next write to the closed pipe.
// Only the adb *client* process is affected; an adb server it starts is not
// our child and is never killed (it serves other tools too).
//
// Cancelling kills the process with its tree (killTree; never an adb
// server: the tree is walked instead of killing a process group or job
// object, because the server an adb client starts may be in those), and
// Wait gives up on output pipes still held open by a grandchild after
// waitDelay (Timing.AdbWaitDelay), so a cancelled call cannot hang (#24).
func adbCmd(ctx context.Context, path string, waitDelay time.Duration, args ...string) *exec.Cmd {
	cmd := procattr.Hide(exec.CommandContext(ctx, path, args...))
	tieToParent(cmd)
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// StartServer runs `adb start-server` outside the recorder's process group
// (a new process group on unix and Windows) so the adb server it may launch
// never shares our group, without the parent-death signal (the server
// outlives the recorder) and without a console window on Windows.
func (a *ADB) StartServer(ctx context.Context) error {
	return startServerCmd(ctx, a.Path).Run()
}

func startServerCmd(ctx context.Context, path string) *exec.Cmd {
	cmd := procattr.Hide(exec.CommandContext(ctx, path, "start-server"))
	ownGroup(cmd)
	return cmd
}
