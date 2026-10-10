//go:build unix

package layout

import (
	"errors"
	"net"
	"syscall"
	"time"
)

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func socketLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
