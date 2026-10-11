//go:build windows

package device

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// listProcs reads the process table with a Toolhelp snapshot.
func listProcs() ([]procEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []procEntry
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		out = append(out, procEntry{pid: int(pe.ProcessID), ppid: int(pe.ParentProcessID), name: windows.UTF16ToString(pe.ExeFile[:])})
	}
	return out, nil
}

// isChild confirms that pid was started after parent: Windows keeps the
// parent id of an orphan, and the id may belong to a newer process by now.
// Without this check a reused pid could make an unrelated process look
// like a descendant of our adb child, and it would be killed. A process
// whose start time cannot be read is not taken for a child (not killed).
func isChild(parent, pid int) bool {
	pc, ok := created(parent)
	if !ok {
		return false
	}
	cc, ok := created(pid)
	return ok && cc >= pc
}

func created(pid int) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0, false
	}
	return c.Nanoseconds(), true
}

func killPid(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	windows.TerminateProcess(h, 1)
	windows.CloseHandle(h)
}
