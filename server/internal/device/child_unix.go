//go:build unix && !linux

package device

import (
	"os/exec"
	"syscall"
)

func tieToParent(*exec.Cmd) {}

func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
