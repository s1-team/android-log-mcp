//go:build darwin

package ipc

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer rejects a peer running as another user (LOCAL_PEERCRED).
func checkPeer(c *net.UnixConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Xucred
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return err
	}
	if gerr != nil {
		return gerr
	}
	return samePeer(int(cred.Uid))
}

func samePeer(uid int) error {
	if uid != os.Getuid() {
		return fmt.Errorf("peer uid %d is not the current user (uid %d)", uid, os.Getuid())
	}
	return nil
}
