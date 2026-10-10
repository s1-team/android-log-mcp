//go:build unix && !linux

package device

import (
	"os/exec"
	"syscall"
)

func tieToParent(*exec.Cmd) {}

func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
