//go:build !unix && !windows

package service

import "os"

func terminateProc(pid int) error { return nil }

func killProc(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

const hasGracefulSignal = false
