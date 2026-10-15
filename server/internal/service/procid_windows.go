//go:build windows

package service

import (
	"golang.org/x/sys/windows"
)

// inspectProc reads the creation time (FILETIME as int64) and the image
// path of pid. ok is false when pid does not exist or has exited.
func inspectProc(pid int) (start int64, exe string, ok bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, "", false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 { // STILL_ACTIVE
		return 0, "", false
	}
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, "", false
	}
	start = int64(c.HighDateTime)<<32 | int64(c.LowDateTime)
	buf := make([]uint16, windows.MAX_PATH*4)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
		exe = windows.UTF16ToString(buf[:n])
	}
	return start, exe, true
}
