package device

import (
	"context"
	"os/exec"
)

// adbCmd builds an adb child process of the recorder. It is tied to the
// recorder's lifetime: cancelling ctx kills it (graceful stop), and on
// Linux it also gets SIGKILL if the recorder dies (Pdeathsig). Elsewhere an
// orphaned `adb logcat` exits on its next write to the closed pipe.
// Only the adb *client* process is affected; an adb server it starts is not
// our child and is never killed (it serves other tools too).
func adbCmd(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	tieToParent(cmd)
	return cmd
}

// StartServer runs `adb start-server` outside the recorder's process group
// (unix) so the adb server it may launch never shares our group, and
// without the parent-death signal: the server outlives the recorder.
func (a *ADB) StartServer(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, a.Path, "start-server")
	ownGroup(cmd)
	return cmd.Run()
}
