//go:build !windows

package procattr

import "os/exec"

// Hide is a no-op outside Windows: child processes there never get a
// window of their own.
func Hide(cmd *exec.Cmd) *exec.Cmd { return cmd }
