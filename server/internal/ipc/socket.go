package ipc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// maxSocketPath is the longest socket path used as is: sun_path holds 104
// bytes on macOS and 108 on Linux and Windows, including the terminator.
const maxSocketPath = 100

// SocketPath is the service socket for the run directory runDir:
// runDir/service.sock, or, when that is too long for sun_path, a short
// per-user path derived from runDir (unix: /tmp/alm-<uid>/<hash>.sock;
// Windows: %TEMP%\alm-<hash>\s.sock). It is a pure function of runDir, so
// every client finds the service without reading any file.
func SocketPath(runDir string) string {
	p := filepath.Join(runDir, "service.sock")
	if len(p) <= maxSocketPath {
		return p
	}
	sum := sha256.Sum256([]byte(filepath.Clean(runDir)))
	h := hex.EncodeToString(sum[:])[:16]
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), "alm-"+h, "s.sock")
	}
	return filepath.Join("/tmp", fmt.Sprintf("alm-%d", os.Getuid()), h+".sock")
}

// ErrUnsupported is returned when the OS has no AF_UNIX sockets.
var ErrUnsupported = errors.New("this system has no AF_UNIX socket support, which the android-log-mcp background service needs: Windows 10 version 1803 / Windows Server 2019 or later is required (older Windows is not supported)")

// PrepareDir creates dir for the socket with current-user-only access:
// mode 0700 (unix; an existing directory owned by another user is refused),
// a protected user-only ACL on Windows.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return restrictDir(dir)
}

// Listen listens on the socket at path. The caller must hold the service
// lock: an existing socket file there is stale and removed first. The
// socket is made 0600 and its directory current-user only. Accepted
// connections from another user are rejected (where the OS reports the
// peer).
func Listen(path string) (net.Listener, error) {
	if err := PrepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		if unsupported(err) {
			return nil, ErrUnsupported
		}
		return nil, err
	}
	if err := restrictSocket(path); err != nil {
		l.Close()
		return nil, err
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	return &peerListener{Listener: l}, nil
}

func dial(path string, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil && unsupported(err) {
		return nil, ErrUnsupported
	}
	return c, err
}

// PeerError is returned by Accept for a peer of another user (the server
// skips it).
type PeerError struct{ Reason string }

func (e *PeerError) Error() string { return "ipc: rejected peer: " + e.Reason }

type peerListener struct{ net.Listener }

func (l *peerListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if uc, ok := c.(*net.UnixConn); ok {
		if err := checkPeer(uc); err != nil {
			c.Close()
			return nil, &PeerError{err.Error()}
		}
	}
	return c, nil
}
