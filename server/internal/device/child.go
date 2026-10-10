package device

import (
	"context"
	"os/exec"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// adbCmd builds an adb child process of the recorder. It is tied to the
// recorder's lifetime and never opens a console window on Windows
// (procattr.Hide): cancelling ctx kills it (graceful stop), and on
// Linux it also gets SIGKILL if the recorder dies (Pdeathsig). Elsewhere an
// orphaned `adb logcat` exits on its next write to the closed pipe.
// Only the adb *client* process is affected; an adb server it starts is not
// our child and is never killed (it serves other tools too).
func adbCmd(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := procattr.Hide(exec.CommandContext(ctx, path, args...))
	tieToParent(cmd)
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
