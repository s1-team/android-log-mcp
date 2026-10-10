//go:build windows

package service

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the service detached from the client's console and process
// group and, where the client's job object allows it, outside that job (an
// MCP client whose job kills its children on exit must not take the service
// along). If breaking away is not allowed it starts inside the job.
func detach(cmd *exec.Cmd) error {
	base := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: base | windows.CREATE_BREAKAWAY_FROM_JOB}
	err := cmd.Start()
	if err != nil && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		c2 := exec.Command(cmd.Path, cmd.Args[1:]...)
		c2.Env, c2.Dir = cmd.Env, cmd.Dir
		c2.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: base}
		if err = c2.Start(); err == nil {
			*cmd = *c2
		}
	}
	return err
}
