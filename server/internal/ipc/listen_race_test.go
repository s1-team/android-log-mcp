package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Stop→Start on one path (the WantedGarbage Start/Stop loop) must not flake
// on chmod ENOENT after bind (#287).
func TestListenCloseRelistenSerial(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		dir := t.TempDir()
		p := SocketPath(filepath.Join(dir, "run"))
		l, err := Listen(p)
		if err != nil {
			t.Fatalf("iter %d listen: %v", i, err)
		}
		l.Close()
		l2, err := Listen(p)
		if err != nil {
			t.Fatalf("iter %d relisten: %v", i, err)
		}
		l2.Close()
	}
}

// Closing a listener whose sock name was already taken must not unlink the
// successor's file. net.Listen defaults to UnlinkOnClose(true); Listen now
// keeps it false until restrictSocket succeeds so a failed attempt cannot
// delete another Listen's sock (#287).
func TestListenFailedCloseDoesNotUnlinkSuccessor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := SocketPath(filepath.Join(dir, "run"))
	if err := PrepareDir(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	ul, ok := raw.(*net.UnixListener)
	if !ok {
		raw.Close()
		t.Fatal("not *net.UnixListener")
	}
	// Same as Listen after bind: do not unlink on Close yet.
	ul.SetUnlinkOnClose(false)

	// Steal the name the way a concurrent Listen's removeStaleSocket would.
	if err := os.Remove(p); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	succ, err := net.Listen("unix", p)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer succ.Close()

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("prior Close unlinked successor sock: %v", err)
	}
	c, err := Dial(p, time.Second)
	if err != nil {
		t.Fatalf("successor not dialable after prior Close: %v", err)
	}
	c.Close()
}
