package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// ErrRunning: another service holds run/service.lock.
var ErrRunning = errors.New("an android-log-mcp service is already running for this user")

// Options configures a service.
type Options struct {
	Control Control
	Version string
	// Root is the data root; "" = the configured root (config.json) or
	// the control directory.
	Root string
	// Embedded is non-empty when the service runs inside an MCP client
	// process because a detached start failed; it holds the reason.
	Embedded string
	// Cfg is the store configuration (retention etc., from the env).
	Cfg store.Config
	// Log, if set, is the service log (moved along with set-root).
	Log *LogFile
	// Drain bounds how long in-flight calls may finish at shutdown
	// (default 5 s).
	Drain time.Duration
}

// Service is a running service.
type Service struct {
	opt     Options
	lock    *os.File
	srv     *ipc.Server
	started time.Time
	exe     string

	opMu  sync.RWMutex // calls/reports: R; set-root: W
	mu    sync.Mutex
	root  string
	m     *device.Manager
	mstop context.CancelFunc
	tools *mcpserver.Tools

	clients   int
	lingerT   *time.Timer
	lingerGen int
	idleSince time.Time
	idleFor   time.Duration
	stopOnce  sync.Once
	stopping  chan struct{}
	done      chan struct{}
	reason    string
}

// Hello is the first request of a client.
type Hello struct {
	Proto   int    `json:"proto"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Role    string `json:"role"` // mcp, call, report, set-root, cli
}

// HelloReply describes the service to the client.
type HelloReply struct {
	Proto        int                 `json:"proto"`
	MinProto     int                 `json:"min_proto"`
	Version      string              `json:"version"`
	PID          int                 `json:"pid"`
	Root         string              `json:"root"`
	Embedded     bool                `json:"embedded,omitempty"`
	Tools        []mcpserver.ToolDef `json:"tools"`
	Instructions string              `json:"instructions"`
}

// CallArgs / CallReply are the "call" op.
type CallArgs struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}
type CallReply struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// Info is the "status" op's reply.
type Info struct {
	PID      int       `json:"pid"`
	Version  string    `json:"version"`
	Proto    int       `json:"proto"`
	MinProto int       `json:"min_proto"`
	Root     string    `json:"root"`
	Control  string    `json:"control"`
	Socket   string    `json:"socket"`
	Exe      string    `json:"exe,omitempty"`
	Log      string    `json:"log,omitempty"`
	Started  time.Time `json:"started"`
	Clients  int       `json:"clients"`
	Embedded string    `json:"embedded,omitempty"`
	Linger   string    `json:"linger,omitempty"`
}

// Text renders the info for logcat_status / `service status`.
func (i Info) Text() string {
	var b strings.Builder
	mode := "background service"
	if i.Embedded != "" {
		mode = "in-process service (" + i.Embedded + ")"
	}
	fmt.Fprintf(&b, "service: %s pid %d, version %s, protocol %d, up %s, clients %d\n", mode, i.PID, i.Version, i.Proto, time.Since(i.Started).Round(time.Second), i.Clients)
	fmt.Fprintf(&b, "root: %s\n", i.Root)
	if i.Linger != "" {
		fmt.Fprintf(&b, "linger: %s\n", i.Linger)
	}
	if i.Log != "" {
		fmt.Fprintf(&b, "service log: %s\n", i.Log)
	}
	return b.String()
}

// Start acquires the service lock, starts recording into the data root and
// serves the socket. It fails with ErrRunning when a service runs.
func Start(opt Options) (*Service, error) {
	if opt.Control.Dir == "" {
		opt.Control = DefaultControl()
	}
	if opt.Drain <= 0 {
		opt.Drain = 5 * time.Second
	}
	if err := ipc.PrepareDir(opt.Control.Run()); err != nil {
		return nil, fmt.Errorf("run directory: %w", err)
	}
	lock, ok := store.TryLock(opt.Control.LockPath())
	if !ok {
		return nil, ErrRunning
	}
	s := &Service{opt: opt, lock: lock, started: time.Now(), stopping: make(chan struct{}), done: make(chan struct{})}
	s.exe, _ = os.Executable()
	root := opt.Root
	if root == "" {
		root = opt.Control.Dir
	}
	if opt.Log != nil && opt.Log.Path() == "" {
		opt.Log.SetRoot(root)
	}
	s.startManager(root)
	l, err := ipc.Listen(opt.Control.Socket())
	if err != nil {
		s.stopManager()
		store.ReleaseLock(lock)
		return nil, err
	}
	s.srv = &ipc.Server{Handler: s.handle, OnConn: s.onConn}
	go s.srv.Serve(l)
	s.mu.Lock()
	n := s.clients
	s.mu.Unlock()
	if n == 0 {
		s.clientsChanged(0) // a service nobody connects to does not stay forever
	}
	if err := s.writeState(); err != nil {
		log.Printf("service: writing %s: %v", opt.Control.StatePath(), err)
	}
	log.Printf("service started: pid %d, version %s, protocol %d, root %s, socket %s", os.Getpid(), opt.Version, Proto, root, opt.Control.Socket())
	return s, nil
}

func (s *Service) startManager(root string) {
	ctx, cancel := context.WithCancel(context.Background())
	m := device.NewManager(root, s.opt.Cfg)
	m.Version = s.opt.Version
	if s.exe != "" {
		m.KeepBin = filepath.Dir(s.exe) // never sweep the running binary
	}
	m.Start(ctx)
	t := &mcpserver.Tools{M: m}
	t.Service = func() string { return s.Info().Text() }
	s.mu.Lock()
	s.root, s.m, s.mstop, s.tools = root, m, cancel, t
	s.mu.Unlock()
}

// stopManager stops recording (flushing every store) and waits for it.
func (s *Service) stopManager() {
	s.mu.Lock()
	m, cancel := s.m, s.mstop
	s.mu.Unlock()
	if m == nil {
		return
	}
	cancel()
	m.Close()
}

func (s *Service) writeState() error {
	st := State{PID: os.Getpid(), Socket: s.opt.Control.Socket(), Version: s.opt.Version, Proto: Proto, Root: s.Root(), Exe: s.exe, Embedded: s.opt.Embedded != "", Started: s.started.UTC()}
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(s.opt.Control.StatePath(), append(b, '\n'), 0o600)
}

// Root is the current data root.
func (s *Service) Root() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// Manager is the current device manager (tests).
func (s *Service) Manager() *device.Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m
}

// Info describes the service.
func (s *Service) Info() Info {
	s.mu.Lock()
	n := s.clients
	s.mu.Unlock()
	i := Info{PID: os.Getpid(), Version: s.opt.Version, Proto: Proto, MinProto: MinProto, Root: s.Root(), Control: s.opt.Control.Dir,
		Socket: s.opt.Control.Socket(), Exe: s.exe, Started: s.started, Clients: n, Embedded: s.opt.Embedded}
	if s.opt.Log != nil {
		i.Log = s.opt.Log.Path()
	}
	i.Linger = s.lingerText()
	return i
}

func (s *Service) onConn(c *ipc.Conn, open bool) {
	if open {
		return
	}
	if _, ok := c.Value.(*Hello); ok {
		s.mu.Lock()
		s.clients--
		n := s.clients
		s.mu.Unlock()
		s.clientsChanged(n)
	}
}

func (s *Service) handle(ctx context.Context, c *ipc.Conn, op string, body json.RawMessage) (any, error) {
	switch op {
	case "hello":
		var h Hello
		if err := json.Unmarshal(body, &h); err != nil {
			return nil, err
		}
		if _, ok := c.Value.(*Hello); !ok {
			c.Value = &h
			s.mu.Lock()
			s.clients++
			n := s.clients
			s.mu.Unlock()
			s.clientsChanged(n)
		}
		return HelloReply{Proto: Proto, MinProto: MinProto, Version: s.opt.Version, PID: os.Getpid(), Root: s.Root(),
			Embedded: s.opt.Embedded != "", Tools: mcpserver.ToolDefs(), Instructions: mcpserver.Instructions()}, nil
	case "status":
		return s.Info(), nil
	}
	h, _ := c.Value.(*Hello)
	if h == nil {
		return nil, errors.New("hello required first")
	}
	switch op {
	case "call":
		var a CallArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		s.mu.Lock()
		t := s.tools
		s.mu.Unlock()
		text, isErr := t.Call(ctx, a.Name, a.Args)
		return CallReply{Text: text, IsError: isErr}, nil
	case "stop":
		go s.Stop(fmt.Sprintf("stop requested (pid %d)", h.PID))
		return "stopping", nil
	case "handoff":
		if CompareVersions(h.Version, s.opt.Version) <= 0 {
			return nil, fmt.Errorf("handoff refused: the client (version %s) is not newer than this service (version %s)", h.Version, s.opt.Version)
		}
		go s.Stop(fmt.Sprintf("replaced by a newer client (pid %d, version %s)", h.PID, h.Version))
		return "stopping", nil
	}
	if f := extraOps[op]; f != nil {
		return f(ctx, s, h, body)
	}
	return nil, fmt.Errorf("unknown op %q (service version %s, protocol %d)", op, s.opt.Version, Proto)
}

// extraOps are ops implemented in other files (report, set-root).
var extraOps = map[string]func(ctx context.Context, s *Service, h *Hello, body json.RawMessage) (any, error){}

// Stop shuts the service down (idempotent; returns when it has stopped):
// no new connections, in-flight calls get up to Options.Drain, connected
// clients are disconnected (they reconnect / restart the service), the
// recorders flush and stop, service.json is removed and the lock released.
func (s *Service) Stop(reason string) {
	first := false
	s.stopOnce.Do(func() {
		first = true
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.stopping)
	})
	if !first {
		<-s.done
		return
	}
	log.Printf("service stopping: %s", reason)
	s.srv.CloseListener()
	deadline := time.Now().Add(s.opt.Drain)
	for s.srv.InFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.srv.Close()
	s.stopLinger()
	s.stopManager()
	if st := s.opt.Control.ReadState(); st != nil && st.PID == os.Getpid() {
		os.Remove(s.opt.Control.StatePath())
	}
	store.ReleaseLock(s.lock)
	log.Printf("service stopped")
	close(s.done)
}

// Done is closed once the service has stopped.
func (s *Service) Done() <-chan struct{} { return s.done }

// Stopping is closed when shutdown begins.
func (s *Service) Stopping() <-chan struct{} { return s.stopping }

// Reason is why the service stopped.
func (s *Service) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}
