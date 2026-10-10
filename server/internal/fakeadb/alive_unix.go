//go:build unix

package fakeadb

import "syscall"

// Alive reports whether process pid exists.
func Alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
