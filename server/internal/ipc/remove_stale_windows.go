//go:build windows

package ipc

import (
	"net"
	"os"
	"time"
)

// removeStaleSocket clears path so bind can recreate it. On Windows a
// force-killed AF_UNIX listener leaves an NTFS reparse-point sock file;
// DeleteFile can fail until afunix.sys has seen a connect attempt, and a
// bare bind then returns WSAEINVAL (#285). Dial first, then remove; if
// remove still fails, rename the file aside so the path is free.
func removeStaleSocket(path string) {
	if c, err := net.DialTimeout("unix", path, 20*time.Millisecond); err == nil {
		c.Close()
	}
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return
	}
	stale := path + ".stale"
	_ = os.Remove(stale)
	if os.Rename(path, stale) == nil {
		_ = os.Remove(stale)
	}
}
