package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/procattr"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// NoSpawn makes clients skip the detached start and run the service
// in-process right away (tests of the fallback).
var NoSpawn bool

// ClientOptions configures a Client.
type ClientOptions struct {
	Control Control
	Version string
	Role    string // mcp, call, report, set-root, record, cli (cli does not keep the service alive)
	// StartTimeout bounds starting the service (default 10 s).
	StartTimeout time.Duration
	// Cfg is the store config an in-process service uses.
	Cfg store.Config
	// NoEmbedded: never run the service in this process; a failed
	// detached start is an error (record --for, report: the user asked
	// for recording that must outlive this command).
	NoEmbedded bool
	// RestartPinned: when the connection drops because the service
	// crashed and an unsuspended recording pin is active, start a new
	// service at once instead of on the next call (issue #11 §8).
	RestartPinned bool
	// Getenv reads the environment (default os.Getenv; tests).
	Getenv func(string) string
	// NoDetach runs the service in this process instead of detached (tests;
	// NoSpawn does it for the whole process).
	NoDetach bool
	// Repair tunes waiting for, and replacing, a service that holds the
	// lock but does not answer (#83). Zero values are the defaults.
	Repair RepairTiming
}

// RepairTiming are the waits of the hung-service handling.
type RepairTiming struct {
	SilentFor    time.Duration // how long a service may be silent before it is replaced (10 s)
	StoppingWait time.Duration // how long a service that is shutting down is waited for (30 s)
	TermWait     time.Duration // graceful stop before the kill (10 s)
	KillWait     time.Duration // wait for the lock after the kill (5 s)
	WaiterWait   time.Duration // how long another client's repair is waited for (60 s)
	HelloWait    time.Duration // how long a probe waits for the hello reply (3 s)
}

func (r *RepairTiming) defaults() {
	for p, d := range map[*time.Duration]time.Duration{&r.SilentFor: 10 * time.Second, &r.StoppingWait: 30 * time.Second,
		&r.TermWait: 10 * time.Second, &r.HelloWait: 3 * time.Second, &r.KillWait: 5 * time.Second, &r.WaiterWait: 60 * time.Second} {
		if *p <= 0 {
			*p = d
		}
	}
}

// ErrIncompatible: the running service is newer and no longer speaks this
// client's protocol. The client never replaces a newer service.
type ErrIncompatible struct{ Client, Service HelloReply }

func (e *ErrIncompatible) Error() string {
	return fmt.Sprintf("the running android-log-mcp service (pid %d, version %s, protocol %d; supports protocols %d–%d) cannot serve this MCP or CLI process (version %s, protocol %d). "+
		"Upgrade android-log-mcp (this MCP or CLI process is older than the service), e.g. `npx -y android-log-mcp@latest`, or stop the service with `android-log-mcp service stop` if you really want this older version",
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
	closed   bool
	lg       clientLog
}

// NewClient returns a client; nothing is connected yet.
func NewClient(opt ClientOptions) *Client {
	if opt.Control.Dir == "" {
		opt.Control = DefaultControl()
	}
	if opt.StartTimeout <= 0 {
		opt.StartTimeout = 10 * time.Second
	}
	opt.Repair.defaults()
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
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
		c.infof("connection to the service lost; reconnecting")
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
		conn, err := dialWait(ctx, c.opt.Control, c.opt.Version, c.opt.Role, c.opt.Repair.HelloWait+2*time.Second)
		if err == nil {
			ok, err := c.checkVersion(ctx, conn)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if ok {
				c.conn = conn
				c.started = (c.embedded != nil && conn.Hello.PID == os.Getpid()) || (c.spawned != 0 && conn.Hello.PID == c.spawned)
				rec := ""
				if conn.Hello.Recording != "" {
					rec = ", recording: " + conn.Hello.Recording
				}
				c.infof("using the android-log-mcp service pid %d (version %s, protocol %d, root %s%s)", conn.Hello.PID, conn.Hello.Version, conn.Hello.Proto, conn.Hello.Root, rec)
				if c.opt.RestartPinned {
					go c.watch(conn)
				}
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
		c.infof("the running service (pid %d, version %s) is older than this MCP or CLI process (version %s): replacing it", h.PID, h.Version, c.opt.Version)
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
		c.lastNote = mcpserver.KV("note", "code", "version_skew", "mcp_process", c.opt.Version, "service", h.Version, "protocol", Proto, "compatible", true) + "\n"
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
	// One client repairs, the others wait for its outcome: a waiter only
	// polls whether the service answers and takes the lock when it is free.
	deadline := time.Now().Add(c.opt.Repair.WaiterWait)
	var lock *os.File
	logged := false
	for {
		f, ok := store.TryLock(ctrl.SpawnLockPath())
		if ok {
			lock = f
			break
		}
		if c.probe(ctx) == probeAnswers {
			return nil
		}
		if !logged {
			logged = true
			c.infof("waiting for the MCP or CLI process that is restarting the service (%s)", ctrl.SpawnLockPath())
		}
		if time.Now().After(deadline) {
			return c.fail("another MCP or CLI process has been starting or repairing the android-log-mcp service for more than %s (%s)", c.opt.Repair.WaiterWait, ctrl.SpawnLockPath())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the android-log-mcp service is being started or repaired by another MCP or CLI process; retry in a few seconds (%s): %w", c.logHintTail(), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer store.ReleaseLock(lock)
	if c.probe(ctx) == probeAnswers {
		return nil // started by another client meanwhile
	}
	if Running(ctrl) {
		replaced, err := c.repairHolder(ctx)
		if err != nil {
			return err
		}
		if !replaced {
			return nil // it answers now, or it let go of the lock
		}
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
	case NoSpawn || c.opt.NoDetach:
		reason = "detached start disabled"
	case len(c.spawns) >= 3 && c.opt.NoEmbedded:
		return c.fail("the service process stopped %d times within a minute", len(c.spawns))
	case len(c.spawns) >= 3:
		reason = fmt.Sprintf("the service process stopped %d times within a minute", len(c.spawns))
	default:
		c.spawns = append(c.spawns, now)
		pid, err := c.spawn(ctx)
		if err == nil {
			c.spawned = pid
			return nil
		}
		reason = "detached start failed: " + err.Error()
		if c.opt.NoEmbedded {
			return fmt.Errorf("could not start the android-log-mcp service detached: %v; run `android-log-mcp service` in a terminal and retry", err)
		}
	}
	if c.opt.NoEmbedded {
		return fmt.Errorf("could not start the android-log-mcp service detached (%s); run `android-log-mcp service` in a terminal and retry", reason)
	}
	c.warnf("%s; running the service in this process (pid %d) instead", reason, os.Getpid())
	return c.startEmbedded(reason)
}

func (c *Client) logHint() string {
	if root, err := ConfiguredRoot(c.opt.Control); err == nil {
		return LogDir(root)
	}
	return "the service log"
}

// watch restarts a crashed service at once when an unsuspended recording
// pin is active (a clean stop or a handoff removes service.json; a crash
// leaves it behind).
func (c *Client) watch(conn *Conn) {
	<-conn.Done()
	c.mu.Lock()
	closed, cur := c.closed, c.conn
	c.mu.Unlock()
	if closed || cur != conn {
		return
	}
	time.Sleep(200 * time.Millisecond) // let a clean stop finish removing service.json
	st := c.opt.Control.ReadState()
	if st == nil || Running(c.opt.Control) || !HasActivePin(c.opt.Control, time.Now()) {
		return
	}
	c.warnf("the service (pid %d) ended without shutting down while a recording pin is active: restarting it", st.PID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.ensure(ctx); err != nil {
		c.errorf("restarting the service: %v", err)
	}
}

// waitReachable polls until the service answers a hello (true), timeout
// (false) or exited fires.
func (c *Client) waitReachable(ctx context.Context, timeout time.Duration, exited <-chan struct{}) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.probe(ctx) == probeAnswers {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-exited:
			return c.probe(ctx) == probeAnswers
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
	cmd := procattr.Hide(exec.Command(exe, "service", "--detached"))
	cmd.Dir = c.opt.Control.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := detach(cmd); err != nil {
		return 0, err
	}
	exited := make(chan struct{})
	var werr error
	go func() { werr = cmd.Wait(); close(exited) }() // reaps it if it exits while we run
	if c.waitReachable(ctx, c.opt.StartTimeout, exited) {
		c.infof("started the android-log-mcp service (pid %d, %s)", cmd.Process.Pid, exe)
		return cmd.Process.Pid, nil
	}
	select {
	case <-exited:
		return 0, c.fail("the service exited right after starting (%v)", werr)
	default:
		cmd.Process.Kill()
		return 0, c.fail("the service did not answer within %s", c.opt.StartTimeout)
	}
}

func (c *Client) startEmbedded(reason string) error {
	root, err := ConfiguredRoot(c.opt.Control)
	if err != nil {
		return err
	}
	lf, err := OpenLog(root)
	if err == nil {
		log.SetOutput(lf) // the in-process service's own lines: file only (#84)
		c.lg.setFile(lf)
	} else {
		lf = nil
	}
	s, err := Start(Options{Control: c.opt.Control, Version: c.opt.Version, Root: root, Embedded: "inside MCP process pid " + fmt.Sprint(os.Getpid()) + ": " + reason, Cfg: c.opt.Cfg, Log: lf})
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
	c.closed = true
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if emb != nil {
		emb.Stop("its MCP process is exiting")
	}
}
