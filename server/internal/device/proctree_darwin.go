//go:build darwin

package device

import (
	"bytes"
	"syscall"

	"golang.org/x/sys/unix"
)

// listProcs reads the process table with sysctl kern.proc.all.
func listProcs() ([]procEntry, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]procEntry, 0, len(kps))
	for _, kp := range kps {
		comm := kp.Proc.P_comm[:]
		if i := bytes.IndexByte(comm, 0); i >= 0 {
			comm = comm[:i]
		}
		out = append(out, procEntry{pid: int(kp.Proc.P_pid), ppid: int(kp.Eproc.Ppid), name: string(comm)})
	}
	return out, nil
}

// isChild: on unix an orphan is re-parented, so a parent id is current.
func isChild(parent, pid int) bool { return true }

func killPid(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
