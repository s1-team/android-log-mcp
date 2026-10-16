//go:build windows

package fakeadb

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// startTime is the creation time of pid ("" if it cannot be read).
func startTime(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return ""
	}
	return strconv.FormatInt(c.Nanoseconds(), 10)
}
