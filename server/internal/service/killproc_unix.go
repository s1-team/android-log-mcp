//go:build unix

package service

import "syscall"

// terminateProc asks pid to stop (SIGTERM); the service flushes its stores.
func terminateProc(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// killProc ends pid at once (SIGKILL).
func killProc(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// hasGracefulSignal: SIGTERM reaches the service's signal handler.
const hasGracefulSignal = true
