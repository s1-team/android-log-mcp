//go:build linux

package device

import (
	"os/exec"
	"syscall"
)

func tieToParent(cmd *exec.Cmd) {
	attr(cmd).Pdeathsig = syscall.SIGKILL
}

func ownGroup(cmd *exec.Cmd) {
	attr(cmd).Setpgid = true
}

func attr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}
