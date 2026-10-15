//go:build !linux && !darwin && !windows

package service

import "syscall"

// inspectProc: no start time or image path is available on this OS, so a
// process can never be verified as the service (nothing is killed
// automatically); only liveness is known.
func inspectProc(pid int) (start int64, exe string, ok bool) {
	return 0, "", syscall.Kill(pid, 0) == nil
}
