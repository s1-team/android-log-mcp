//go:build !windows

package service

func consoleProbe() (int, bool) { return 0, false }
