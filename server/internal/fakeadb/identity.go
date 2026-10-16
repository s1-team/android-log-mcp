package fakeadb

import "strconv"

// pidLine is the entry of a fake process in the pids file: "pid" or
// "pid:start time". Windows frees a pid the moment its process is gone and
// hands it to the next new process, so a process that exited and whose pid
// was reused by an unrelated one (another test's fake adb) looked like a
// fake adb that was still running (TestCloseWaitsForSideWorkAdb, #103).
func pidLine(pid int) string {
	if st := startTime(pid); st != "" {
		return strconv.Itoa(pid) + ":" + st
	}
	return strconv.Itoa(pid)
}

// sameProcess reports whether pid is still the process that recorded id
// (always true for an entry with no start time).
func sameProcess(pid int, id string) bool {
	return id == "" || startTime(pid) == id
}
