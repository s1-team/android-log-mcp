//go:build unix

package service

import (
	"os/exec"
	"syscall"
)

// detach starts the service in a new session (setsid): no controlling
// terminal and its own process group, so an MCP client that kills its
// process group on exit does not take the service along.
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
