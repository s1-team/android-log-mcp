//go:build windows

package procattr

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hide makes cmd start without a console window (CREATE_NO_WINDOW) and with
// a hidden main window (SW_HIDE). It keeps any attributes and creation flags
// already set; later code must add to cmd.SysProcAttr, not replace it.
func Hide(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	return cmd
}
