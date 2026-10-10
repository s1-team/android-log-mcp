//go:build linux

package ipc

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer rejects a peer running as another user (SO_PEERCRED).
func checkPeer(c *net.UnixConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
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
