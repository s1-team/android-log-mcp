//go:build linux

package device

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// listProcs reads the process table from /proc.
func listProcs() ([]procEntry, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procEntry
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		// pid (comm) state ppid ...; comm may contain spaces and parens
		s := string(b)
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open {
			continue
		}
		f := strings.Fields(s[end+1:])
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		out = append(out, procEntry{pid: pid, ppid: ppid, name: s[open+1 : end]})
	}
	return out, nil
}

// isChild: on unix an orphan is re-parented, so a parent id is current.
func isChild(parent, pid int) bool { return true }

func killPid(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
