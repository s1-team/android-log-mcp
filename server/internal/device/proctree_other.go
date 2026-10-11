//go:build !linux && !darwin && !windows

package device

import "errors"

// listProcs is not implemented here: only the adb child itself is killed.
func listProcs() ([]procEntry, error) { return nil, errors.New("process table not supported") }

func isChild(parent, pid int) bool { return false }

func killPid(int) {}
