//go:build !unix

package layout

import (
	"net"
	"os"
	"time"
)

// pidAlive: on Windows FindProcess opens a handle and fails for a pid that
// does not exist.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	p.Release()
	return true
}

func socketLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
