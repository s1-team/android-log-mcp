package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// NoSpawn makes clients skip the detached start and run the service
// in-process right away (tests of the fallback).
var NoSpawn bool

// ClientOptions configures a Client.
type ClientOptions struct {
	Control Control
	Version string
	Role    string // mcp, call, report, set-root
	// StartTimeout bounds starting the service (default 10 s).
	StartTimeout time.Duration
	// Cfg is the store config an in-process service uses.
	Cfg store.Config
}

// ErrIncompatible: the running service is newer and no longer speaks this
// client's protocol. The client never replaces a newer service.
type ErrIncompatible struct{ Client, Service HelloReply }

func (e *ErrIncompatible) Error() string {
	return fmt.Sprintf("the running android-log-mcp service (pid %d, version %s, protocol %d; supports protocols %d–%d) cannot serve this client (version %s, protocol %d). "+
		"Upgrade this client, e.g. `npx -y android-log-mcp@latest`, or stop the service with `android-log-mcp service stop` if you really want this older version",
		e.Service.PID, e.Service.Version, e.Service.Proto, e.Service.MinProto, e.Service.Proto, e.Client.Version, e.Client.Proto)
}

// Client is a thin client of the service. It (re)connects on demand: when
// the service is not running it starts it detached (from a copy of this
// binary under <root>/bin), or, if that fails, runs it in this process
// and serves the same socket for other clients; when the service is older
// than this client it replaces it; when the connection drops it reconnects
// and retries the call (all tools are read-only).
type Client struct {
	opt ClientOptions

	mu       sync.Mutex
	conn     *Conn
	embedded *Service
	spawns   []time.Time
	started  bool // this client started the service it last connected to
	spawned  int  // pid of the last service this client started detached
	lastNote string
}

// NewClient returns a client; nothing is connected yet.
func NewClient(opt ClientOptions) *Client {
	if opt.Control.Dir == "" {
		opt.Control = DefaultControl()
	}
	if opt.StartTimeout <= 0 {
		opt.StartTimeout = 10 * time.Second
	}
	return &Client{opt: opt}
}

// Connect connects (starting or replacing the service as needed) and
// returns the service's hello.
func (c *Client) Connect(ctx context.Context) (HelloReply, error) {
	conn, err := c.ensure(ctx)
	if err != nil {
		return HelloReply{}, err
	}
	return conn.Hello, nil
}

// Started reports whether this client started (or became) the service it
// is connected to.
func (c *Client) Started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// Embedded is the in-process service, if this client runs one.
func (c *Client) Embedded() *Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.embedded
}

func alive(conn *Conn) bool {
	if conn == nil {
		return false
	}
	select {
	case <-conn.Done():
		return false
	default:
		return true
	}
}

func (c *Client) ensure(ctx context.Context) (*Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if alive(c.conn) {
		return c.conn, nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		log.Printf("connection to the service lost; reconnecting")
	}
	if c.embedded != nil {
		select {
		case <-c.embedded.Done():
			c.embedded = nil // replaced by a newer client, or stopped
		default:
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := Dial(ctx, c.opt.Control, c.opt.Version, c.opt.Role)
		if err == nil {
			ok, err := c.checkVersion(ctx, conn)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if ok {
				c.conn = conn
				c.started = (c.embedded != nil && conn.Hello.PID == os.Getpid()) || (c.spawned != 0 && conn.Hello.PID == c.spawned)
				log.Printf("using the android-log-mcp service pid %d (version %s, protocol %d, root %s)", conn.Hello.PID, conn.Hello.Version, conn.Hello.Proto, conn.Hello.Root)
				return conn, nil
			}
			continue // replaced an older service: start ours
		}
		if errors.Is(err, ipc.ErrUnsupported) {
			return nil, err
		}
		var re *ipc.RemoteError
		if errors.As(err, &re) {
			return nil, fmt.Errorf("android-log-mcp service: %s", re.Msg)
		}
		lastErr = err
		if err := c.start(ctx); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not connect to the android-log-mcp service (socket %s): %v", c.opt.Control.Socket(), lastErr)
}

// checkVersion applies the version rules: a service older than this client
// is replaced (handoff, then false); a newer or equal one is used if it
// speaks this client's protocol; otherwise an *ErrIncompatible.
func (c *Client) checkVersion(ctx context.Context, conn *Conn) (bool, error) {
	h := conn.Hello
	if CompareVersions(c.opt.Version, h.Version) > 0 {
		log.Printf("the running service (pid %d, version %s) is older than this client (version %s): replacing it", h.PID, h.Version, c.opt.Version)
		err := conn.Do(ctx, "handoff", nil, nil)
		conn.Close()
		if err != nil {
			return false, fmt.Errorf("replacing the older service (pid %d, version %s): %v", h.PID, h.Version, err)
		}
		if !WaitStopped(c.opt.Control, 30*time.Second) {
			return false, fmt.Errorf("the older service (pid %d, version %s) did not stop within 30 s", h.PID, h.Version)
		}
		return false, nil
	}
	if !ProtoCompatible(Proto, h.MinProto, h.Proto) {
		return false, &ErrIncompatible{Client: HelloReply{Version: c.opt.Version, Proto: Proto}, Service: h}
	}
	c.lastNote = ""
	if CompareVersions(c.opt.Version, h.Version) < 0 {
		c.lastNote = fmt.Sprintf("client: version %s, protocol %d (older than the service; compatible)\n", c.opt.Version, Proto)
	}
	return true, nil
}

// start starts the service: detached from a copy of this binary, or in
// this process when that fails. Serialized across clients by spawn.lock;
// returns nil when a service is (now) reachable or starting.
func (c *Client) start(ctx context.Context) error {
	ctrl := c.opt.Control
	if err := ipc.PrepareDir(ctrl.Run()); err != nil {
		return fmt.Errorf("run directory: %w", err)
	}
	deadline := time.Now().Add(c.opt.StartTimeout + 5*time.Second)
	var lock *os.File
	for {
		f, ok := store.TryLock(ctrl.SpawnLockPath())
		if ok {
			lock = f
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another client has been starting the service for too long (%s)", ctrl.SpawnLockPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer store.ReleaseLock(lock)
	if reachable(ctrl) {
		return nil // started by another client meanwhile
	}
	if Running(ctrl) {
		// the lock is held but the socket does not answer: starting up or
		// shutting down
		if waitReachable(ctx, ctrl, c.opt.StartTimeout, nil) || !Running(ctrl) {
			return nil
		}
		st := ctrl.ReadState()
		pid := 0
		if st != nil {
			pid = st.PID
		}
		return fmt.Errorf("an android-log-mcp service (pid %d) holds %s but does not answer on %s; stop that process", pid, ctrl.LockPath(), ctrl.Socket())
	}
	now := time.Now()
	kept := c.spawns[:0]
	for _, t := range c.spawns {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	c.spawns = kept
	reason := ""
	switch {
	case NoSpawn:
		reason = "detached start disabled"
	case len(c.spawns) >= 3:
		reason = fmt.Sprintf("the background service stopped %d times within a minute", len(c.spawns))
	default:
		c.spawns = append(c.spawns, now)
		pid, err := c.spawn(ctx)
		if err == nil {
			c.spawned = pid
			return nil
		}
		reason = "detached start failed: " + err.Error()
	}
	log.Printf("%s; running the service in this process (pid %d) instead", reason, os.Getpid())
	return c.startEmbedded(reason)
}

func reachable(ctrl Control) bool {
	cl, err := ipc.Dial(ctrl.Socket(), time.Second)
	if err != nil {
		return false
	}
	cl.Close()
	return true
}

// waitReachable polls the socket until timeout (false) or exited fires.
func waitReachable(ctx context.Context, ctrl Control, timeout time.Duration, exited <-chan struct{}) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if reachable(ctrl) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-exited:
			return reachable(ctrl)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

func (c *Client) spawn(ctx context.Context) (int, error) {
	root, err := ConfiguredRoot(c.opt.Control)
	if err != nil {
		return 0, err
	}
	exe, err := InstallBinary(root, c.opt.Version)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "service", "--detached")
	cmd.Dir = c.opt.Control.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := detach(cmd); err != nil {
		return 0, err
	}
	exited := make(chan struct{})
	var werr error
	go func() { werr = cmd.Wait(); close(exited) }() // reaps it if it exits while we run
	if waitReachable(ctx, c.opt.Control, c.opt.StartTimeout, exited) {
		log.Printf("started the android-log-mcp service (pid %d, %s)", cmd.Process.Pid, exe)
		return cmd.Process.Pid, nil
	}
	select {
	case <-exited:
		return 0, fmt.Errorf("the service exited right after starting (%v); see %s", werr, LogDir(root))
	default:
		cmd.Process.Kill()
		return 0, fmt.Errorf("the service did not answer within %s; see %s", c.opt.StartTimeout, LogDir(root))
	}
}

func (c *Client) startEmbedded(reason string) error {
	root, err := ConfiguredRoot(c.opt.Control)
	if err != nil {
		return err
	}
	lf, err := OpenLog(root)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	} else {
		lf = nil
	}
	s, err := Start(Options{Control: c.opt.Control, Version: c.opt.Version, Root: root, Embedded: "inside MCP client pid " + fmt.Sprint(os.Getpid()) + ": " + reason, Cfg: c.opt.Cfg, Log: lf})
	if errors.Is(err, ErrRunning) {
		return nil // another client won the race
	}
	if err != nil {
		return err
	}
	c.embedded = s
	return nil
}

// Do runs op on the service (reconnecting once if the connection was lost
// before the request was answered).
func (c *Client) Do(ctx context.Context, op string, in, out any) error {
	for attempt := 0; ; attempt++ {
		conn, err := c.ensure(ctx)
		if err != nil {
			return err
		}
		err = conn.Do(ctx, op, in, out)
		if errors.Is(err, ipc.ErrClosed) && attempt < 2 {
			continue
		}
		return err
	}
}

// Call runs a tool on the service. A dropped connection (the service was
// stopped, replaced or crashed) is retried after reconnecting: the tools
// are read-only.
func (c *Client) Call(ctx context.Context, name string, raw []byte) (string, bool) {
	var r CallReply
	err := c.Do(ctx, "call", CallArgs{Name: name, Args: raw}, &r)
	if err != nil {
		return "android-log-mcp: " + err.Error(), true
	}
	if name == "logcat_status" {
		c.mu.Lock()
		note := c.lastNote
		c.mu.Unlock()
		if note != "" {
			r.Text = note + r.Text
		}
	}
	return r.Text, r.IsError
}

// Close disconnects; an in-process service is stopped (its other clients
// reconnect and start a new one).
func (c *Client) Close() {
	c.mu.Lock()
	conn, emb := c.conn, c.embedded
	c.conn, c.embedded = nil, nil
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if emb != nil {
		emb.Stop("its MCP client process is exiting")
	}
}
