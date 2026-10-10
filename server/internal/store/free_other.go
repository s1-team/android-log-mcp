//go:build !linux && !darwin && !windows

package store

func freeSpace(string) (uint64, error) { return 0, errNoFreeSpace }
