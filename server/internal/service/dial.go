package service

import (
	"context"
	"os"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
)

// Conn is a connection to the service after a successful hello.
type Conn struct {
	*ipc.Client
	Hello HelloReply
}

// Dial connects to the running service of ctrl and says hello (no start,
// no version handling; see Connect for that).
func Dial(ctx context.Context, ctrl Control, version, role string) (*Conn, error) {
	c, err := ipc.Dial(ctrl.Socket(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	var r HelloReply
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Do(hctx, "hello", Hello{Proto: Proto, Version: version, PID: os.Getpid(), Role: role}, &r); err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{Client: c, Hello: r}, nil
}

// Running reports whether a service holds the lock of ctrl.
func Running(ctrl Control) bool {
	return lockHeld(ctrl.LockPath())
}

// WaitStopped waits until no service holds the lock (true) or timeout.
func WaitStopped(ctrl Control, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !Running(ctrl) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
