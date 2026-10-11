//go:build unix && !linux && !darwin

package fakeadb

import "syscall"

// Alive reports whether process pid exists (a zombie counts as alive here).
func Alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Exiting is not detectable here.
func Exiting(int) bool { return false }
