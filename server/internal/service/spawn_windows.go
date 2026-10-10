//go:build windows

package service

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// detachFlags start the service in its own process group with its own,
// windowless console (cmd already carries procattr.Hide, i.e.
// CREATE_NO_WINDOW). Not DETACHED_PROCESS: a process without any console
// makes Windows open a new, visible console window for every console child
// it starts (adb.exe), which is exactly the pop-up storm 0.4.0 had. With a
// hidden console of its own the service is still detached from the client's
// console (Ctrl+C or closing the client's window does not reach it), and
// its children inherit the hidden console.
const detachFlags = windows.CREATE_NEW_PROCESS_GROUP

// detach starts the service detached from the client's console and process
// group and, where the client's job object allows it, outside that job (an
// MCP client whose job kills its children on exit must not take the service
// along). If breaking away is not allowed it starts inside the job.
func detach(cmd *exec.Cmd) error {
	procattr.Hide(cmd)
	base := cmd.SysProcAttr.CreationFlags | detachFlags
	cmd.SysProcAttr.CreationFlags = base | windows.CREATE_BREAKAWAY_FROM_JOB
	err := cmd.Start()
	if err != nil && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		c2 := procattr.Hide(exec.Command(cmd.Path, cmd.Args[1:]...))
		c2.Env, c2.Dir = cmd.Env, cmd.Dir
		c2.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: base}
		if err = c2.Start(); err == nil {
			*cmd = *c2
		}
	}
	return err
}
