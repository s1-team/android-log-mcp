//go:build !windows

package fakeadb

// startTime is only needed where pids are reused at once (Windows).
func startTime(int) string { return "" }
