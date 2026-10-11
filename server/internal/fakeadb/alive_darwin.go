//go:build darwin

package fakeadb

import "golang.org/x/sys/unix"

// From <sys/proc.h>.
const (
	sZomb  = 5      // SZOMB: exited, waiting to be reaped
	pWExit = 0x2000 // P_WEXIT: working on exiting
)

func kinfo(pid int) (*unix.KinfoProc, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k == nil || int(k.Proc.P_pid) != pid {
		return nil, false
	}
	return k, true
}

// Alive reports whether process pid exists and has not exited: a zombie
// (killed or exited but not yet reaped) is not alive; every other state
// counts as alive, including a process that is still exiting (Exiting).
func Alive(pid int) bool {
	k, ok := kinfo(pid)
	return ok && k.Proc.P_stat != sZomb
}

// Exiting reports whether pid is alive but already tearing itself down
// (P_WEXIT): it will become a zombie on its own without running user code.
func Exiting(pid int) bool {
	k, ok := kinfo(pid)
	return ok && k.Proc.P_stat != sZomb && k.Proc.P_flag&pWExit != 0
}
