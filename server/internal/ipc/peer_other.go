//go:build !linux && !darwin

package ipc

import "net"

// checkPeer: no portable peer credentials here (Windows, BSDs); access is
// limited by the directory's permissions / ACL.
func checkPeer(*net.UnixConn) error { return nil }
