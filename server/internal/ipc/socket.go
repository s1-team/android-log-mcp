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
// runDir/service.sock, or, when that is longer than maxSocketPath, a short
// per-user path derived from runDir (unix: /tmp/alm-<uid>/<hash>.sock;
// Windows: %TEMP%\alm-<hash>\s.sock). It is a pure function of runDir, so
// every client finds the service without reading any file. A service
// started by any 0.6.x or 0.7.x version uses the same path.
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
var ErrUnsupported = errors.New("this system has no AF_UNIX socket support, which the android-log-mcp service process needs: Windows 10 version 1803 / Windows Server 2019 or later is required (older Windows is not supported)")

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
//
// On Windows, bind against a leftover sock file (for example after a
// force-killed service) returns WSAEINVAL rather than address-in-use. A
// first remove can also lose a race with the kernel releasing the name; if
// bind still fails and the path is present, clear it and retry once (#285).
//
// After a successful bind, the sock name can still vanish before we chmod
// it (another Listen's removeStaleSocket, or Close with UnlinkOnClose on a
// prior listener for the same path). net.Listen's default UnlinkOnClose is
// true, so closing a listener whose name was already stolen would delete
// the successor's sock and turn a race into chmod ENOENT for both. We keep
// UnlinkOnClose false until restrictSocket succeeds, and retry once when
// the name is gone (#287 CI flake on TestWantedGarbageAndUnknownKeys).
func Listen(path string) (net.Listener, error) {
	if err := PrepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		removeStaleSocket(path)
		l, err := net.Listen("unix", path)
		if err != nil {
			lastErr = err
			if _, statErr := os.Lstat(path); statErr == nil {
				// Leftover name (Windows WSAEINVAL path): clear and retry.
				continue
			}
			break
		}
		if ul, ok := l.(*net.UnixListener); ok {
			// Own the FD only; do not unlink on Close until the path name
			// is confirmed ours (avoids deleting a concurrent successor).
			ul.SetUnlinkOnClose(false)
		}
		if _, statErr := os.Lstat(path); statErr != nil {
			l.Close()
			lastErr = statErr
			continue
		}
		if err := restrictSocket(path); err != nil {
			l.Close()
			lastErr = err
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if ul, ok := l.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(true)
		}
		return &peerListener{Listener: l}, nil
	}
	if lastErr != nil && unsupported(lastErr) {
		return nil, ErrUnsupported
	}
	return nil, lastErr
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
