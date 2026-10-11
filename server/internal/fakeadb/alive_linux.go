//go:build linux

package fakeadb

import (
	"os"
	"strconv"
	"strings"
)

// pfExiting is the kernel's PF_EXITING task flag: the process got a fatal
// signal or called exit and is tearing itself down; it never runs user
// code again.
const pfExiting = 0x4

// procStat returns the state letter and the flags of pid from
// /proc/<pid>/stat (ok false when the process is gone).
func procStat(pid int) (state byte, flags uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseStat(string(b))
}

// parseStat parses a /proc/<pid>/stat line: pid (comm) state ppid pgrp
// session tty_nr tpgid flags ...; comm may contain spaces and
// parentheses, so fields are read after the last ')'.
func parseStat(s string) (state byte, flags uint64, ok bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 7 || len(f[0]) != 1 {
		return 0, 0, false
	}
	flags, err := strconv.ParseUint(f[6], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return f[0][0], flags, true
}

// Alive reports whether process pid exists and has not exited: a zombie
// (Z, killed or exited but not yet reaped by its parent or the reaper of
// orphans) or a dead (X) process is not alive. Every other state counts
// as alive, including D and a process that is still exiting (Exiting).
func Alive(pid int) bool {
	st, _, ok := procStat(pid)
	return ok && st != 'Z' && st != 'X'
}

// Exiting reports whether pid is alive but already tearing itself down
// (PF_EXITING): it will reach Z on its own without running user code.
func Exiting(pid int) bool {
	st, flags, ok := procStat(pid)
	return ok && st != 'Z' && st != 'X' && flags&pfExiting != 0
}
