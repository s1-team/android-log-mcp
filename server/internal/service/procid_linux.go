//go:build linux

package service

import (
	"os"
	"strconv"
	"strings"
)

// inspectProc reads the OS start time (clock ticks since boot, /proc stat
// field 22) and the executable of pid. ok is false when pid does not exist
// or is a zombie / dead.
func inspectProc(pid int) (start int64, exe string, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the name may contain spaces and ')'
	if i < 0 {
		return 0, "", false
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); starttime is field 22 = f[19]
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return 0, "", false
	}
	start, err = strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return 0, "", false
	}
	exe, _ = os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	return start, strings.TrimSuffix(exe, " (deleted)"), true
}
