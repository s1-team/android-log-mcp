//go:build darwin

package service

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// inspectProc reads the OS start time (kern.proc.pid p_starttime, in ns) and
// the executable path (kern.procargs2) of pid. ok is false when pid does not
// exist or is a zombie.
func inspectProc(pid int) (start int64, exe string, ok bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k == nil || int(k.Proc.P_pid) != pid || k.Proc.P_stat == 5 { // SZOMB
		return 0, "", false
	}
	start = k.Proc.P_starttime.Sec*1e9 + int64(k.Proc.P_starttime.Usec)*1e3
	// kern.procargs2: int32 argc, then the NUL-terminated exec path
	if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil && len(raw) > 4 {
		rest := raw[4:]
		if j := bytes.IndexByte(rest, 0); j > 0 {
			exe = string(rest[:j])
		}
	}
	return start, exe, true
}
