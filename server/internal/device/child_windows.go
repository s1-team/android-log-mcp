//go:build windows

package device

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// tieToParent: Windows has no parent-death signal; an orphaned adb client
// exits on its next write to the closed pipe.
func tieToParent(*exec.Cmd) {}

// ownGroup starts cmd in a new process group, so console control events
// sent to our group never reach the adb server it may launch. Flags set
// earlier (procattr.Hide) are kept.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}
