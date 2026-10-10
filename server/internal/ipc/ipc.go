// Package ipc is the local transport between android-log-mcp clients (the
// MCP stdio processes, `call`, `report`, `set-root`) and the single
// background service (issue #3).
//
// Transport: an AF_UNIX stream socket on every platform (Windows 10 1803 /
// Server 2019 or later). There is no TCP fallback and no token: access is
// limited to the current user by the permissions of the socket's directory
// (0700, a user-only ACL on Windows) and the socket (0600), plus a
// same-user peer check where the OS reports the peer (Linux SO_PEERCRED,
// macOS LOCAL_PEERCRED).
//
// Framing: one JSON object per line, at most MaxFrame bytes. Requests carry
// an id; responses echo it, so one connection carries concurrent calls.
// The op "cancel" with body {"id":N} cancels request N.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MaxFrame bounds one message (tool outputs are far smaller; reports send
// only their summary).
const MaxFrame = 4 << 20

// Request is a client → service message.
type Request struct {
	ID   uint64          `json:"id"`
	Op   string          `json:"op"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Response is a service → client message.
type Response struct {
	ID    uint64          `json:"id"`
	Body  json.RawMessage `json:"body,omitempty"`
	Error string          `json:"error,omitempty"`
}

// RemoteError is an error returned by the service's handler.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// ErrClosed means the connection to the service is gone (the service
// exited or was replaced); callers reconnect.
var ErrClosed = errors.New("connection to the android-log-mcp service closed")

// ---- framing ----

type frameConn struct {
	c  net.Conn
	sc *bufio.Scanner
	wm sync.Mutex
}

func newFrameConn(c net.Conn) *frameConn {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), MaxFrame+1)
	return &frameConn{c: c, sc: sc}
}

func (f *frameConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("ipc: message of %d bytes exceeds the %d byte limit", len(b), MaxFrame)
	}
	f.wm.Lock()
	defer f.wm.Unlock()
	f.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = f.c.Write(append(b, '\n'))
	return err
}

func (f *frameConn) recv(v any) error {
	if !f.sc.Scan() {
		if err := f.sc.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	return json.Unmarshal(f.sc.Bytes(), v)
}

// ---- client ----

// Client is one connection to the service.
type Client struct {
	fc      *frameConn
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan Response
	done    chan struct{}
	err     error
}

// Dial connects to the service socket at path.
func Dial(path string, timeout time.Duration) (*Client, error) {
	c, err := dial(path, timeout)
	if err != nil {
		return nil, err
	}
	cl := &Client{fc: newFrameConn(c), pending: map[uint64]chan Response{}, done: make(chan struct{})}
	go cl.readLoop()
	return cl, nil
}

func (c *Client) readLoop() {
	var err error
	for {
		var r Response
		if err = c.fc.recv(&r); err != nil {
			break
		}
		c.mu.Lock()
		ch := c.pending[r.ID]
		delete(c.pending, r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

// Done is closed when the connection is gone.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close closes the connection.
func (c *Client) Close() error { return c.fc.c.Close() }

// Do sends op with body in and decodes the response body into out (if
// non-nil). A cancelled ctx sends "cancel" and returns ctx.Err(). A lost
// connection returns ErrClosed; a handler error a *RemoteError.
func (c *Client) Do(ctx context.Context, op string, in, out any) error {
	var body json.RawMessage
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	ch := make(chan Response, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.fc.send(Request{ID: id, Op: op, Body: body}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		select {
		case <-c.done:
			return ErrClosed
		default:
		}
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if r.Error != "" {
			return &RemoteError{r.Error}
		}
		if out != nil && len(r.Body) > 0 {
			return json.Unmarshal(r.Body, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.fc.send(Request{Op: "cancel", Body: json.RawMessage(fmt.Sprintf(`{"id":%d}`, id))})
		return ctx.Err()
	}
}

// ---- server ----

// Handler serves one request. The returned value is JSON-encoded as the
// response body; an error becomes the response's error text.
type Handler func(ctx context.Context, c *Conn, op string, body json.RawMessage) (any, error)

// Conn is a client connection on the service side.
type Conn struct {
	ID     uint64
	fc     *frameConn
	mu     sync.Mutex
	cancel map[uint64]context.CancelFunc
	// Value holds per-connection state of the handler (e.g. the hello).
	Value any
}

// Close drops the connection.
func (c *Conn) Close() error { return c.fc.c.Close() }

// Server accepts connections and dispatches requests to Handler.
type Server struct {
	Handler Handler
	// MaxConns bounds concurrent connections (default 64); more are
	// refused with an error message.
	MaxConns int
	// OnConn is called when a connection opens (open=true) and closes.
	OnConn func(c *Conn, open bool)

	mu     sync.Mutex
	l      net.Listener
	conns  map[*Conn]bool
	nextID uint64
	wg     sync.WaitGroup
	closed bool
	busy   atomic.Int64
}

// InFlight is the number of requests whose handler is running or whose
// reply has not been written yet.
func (s *Server) InFlight() int64 { return s.busy.Load() }

// Serve accepts on l until Close.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return net.ErrClosed
	}
	s.l = l
	if s.conns == nil {
		s.conns = map[*Conn]bool{}
	}
	s.mu.Unlock()
	max := s.MaxConns
	if max <= 0 {
		max = 64
	}
	for {
		nc, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			var pe *PeerError
			if errors.As(err, &pe) {
				continue // rejected peer (another user)
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return nil
		}
		if len(s.conns) >= max {
			s.mu.Unlock()
			fc := newFrameConn(nc)
			fc.send(Response{Error: fmt.Sprintf("too many connections (limit %d)", max)})
			nc.Close()
			continue
		}
		s.nextID++
		c := &Conn{ID: s.nextID, fc: newFrameConn(nc), cancel: map[uint64]context.CancelFunc{}}
		s.conns[c] = true
		s.wg.Add(1)
		s.mu.Unlock()
		if s.OnConn != nil {
			s.OnConn(c, true)
		}
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c *Conn) {
	defer s.wg.Done()
	ctx, cancelAll := context.WithCancel(context.Background())
	var reqs sync.WaitGroup
	for {
		var r Request
		if err := c.fc.recv(&r); err != nil {
			break
		}
		if r.Op == "cancel" {
			var b struct {
				ID uint64 `json:"id"`
			}
			json.Unmarshal(r.Body, &b)
			c.mu.Lock()
			if f := c.cancel[b.ID]; f != nil {
				f()
			}
			c.mu.Unlock()
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancel[r.ID] = cancel
		c.mu.Unlock()
		reqs.Add(1)
		s.busy.Add(1)
		go func(r Request) {
			defer reqs.Done()
			defer s.busy.Add(-1)
			defer func() {
				c.mu.Lock()
				delete(c.cancel, r.ID)
				c.mu.Unlock()
				cancel()
			}()
			v, err := s.Handler(rctx, c, r.Op, r.Body)
			resp := Response{ID: r.ID}
			if err != nil {
				resp.Error = err.Error()
			} else if v != nil {
				b, merr := json.Marshal(v)
				if merr != nil {
					resp.Error = merr.Error()
				} else {
					resp.Body = b
				}
			}
			if serr := c.fc.send(resp); serr != nil && resp.Error == "" && !errors.Is(serr, net.ErrClosed) {
				c.fc.send(Response{ID: r.ID, Error: serr.Error()}) // e.g. too large
			}
		}(r)
	}
	cancelAll()
	c.fc.c.Close()
	reqs.Wait()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	if s.OnConn != nil {
		s.OnConn(c, false)
	}
}

// Conns is the number of open connections.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Close stops accepting, closes every connection and waits for their
// handlers to return.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	l := s.l
	var cs []*Conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	if l != nil {
		l.Close()
	}
	for _, c := range cs {
		c.fc.c.Close()
	}
	s.wg.Wait()
}

// CloseListener stops accepting new connections (existing ones stay).
func (s *Server) CloseListener() {
	s.mu.Lock()
	s.closed = true
	l := s.l
	s.mu.Unlock()
	if l != nil {
		l.Close()
	}
}
