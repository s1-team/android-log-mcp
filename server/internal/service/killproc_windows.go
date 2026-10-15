//go:build windows

package service

import "os"

// terminateProc: a detached service has no console signal that reaches it
// (see spawn_windows.go), so there is no graceful signal; the caller uses
// the stop op over the socket instead.
func terminateProc(pid int) error { return nil }

// killProc is TerminateProcess; nothing is flushed.
func killProc(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

const hasGracefulSignal = false
