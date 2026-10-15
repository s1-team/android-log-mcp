package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir returns a temp dir whose socket path fits sun_path on every OS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "almipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "run")
}

func echoServer(t *testing.T, path string, s *Server) {
	t.Helper()
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Handler == nil {
		s.Handler = func(ctx context.Context, c *Conn, op string, body json.RawMessage) (any, error) {
			switch op {
			case "echo":
				return json.RawMessage(body), nil
			case "fail":
				return nil, errors.New("boom")
			case "block":
				<-ctx.Done()
				return nil, ctx.Err()
			case "big":
				return strings.Repeat("x", MaxFrame+10), nil
			}
			return nil, errors.New("unknown op " + op)
		}
	}
	go s.Serve(l)
	t.Cleanup(s.Close)
}

func TestRoundTripConcurrentAndErrors(t *testing.T) {
	p := SocketPath(shortDir(t))
	echoServer(t, p, &Server{})
	c, err := Dial(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out map[string]int
			if err := c.Do(context.Background(), "echo", map[string]int{"n": i}, &out); err != nil || out["n"] != i {
				t.Errorf("echo %d: %v %v", i, out, err)
			}
		}(i)
	}
	wg.Wait()
	var re *RemoteError
	if err := c.Do(context.Background(), "fail", nil, nil); !errors.As(err, &re) || re.Msg != "boom" {
		t.Fatalf("fail: %v", err)
	}
	// a response over the frame limit becomes an error, the connection survives
	if err := c.Do(context.Background(), "big", nil, nil); !errors.As(err, &re) || !strings.Contains(re.Msg, "limit") {
		t.Fatalf("big: %v", err)
	}
	if err := c.Do(context.Background(), "echo", 1, nil); err != nil {
		t.Fatalf("after big: %v", err)
	}
}

func TestCancelAndClose(t *testing.T) {
	p := SocketPath(shortDir(t))
	s := &Server{}
	echoServer(t, p, s)
	c, err := Dial(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Do(ctx, "block", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("block: %v", err)
	}
	// service goes away: pending and later calls fail with ErrClosed
	go func() { time.Sleep(50 * time.Millisecond); s.Close() }()
	if err := c.Do(context.Background(), "block", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed")
	}
	if _, err := Dial(p, 200*time.Millisecond); err == nil {
		t.Fatal("dial after close succeeded")
	}
}

func TestMaxConns(t *testing.T) {
	p := SocketPath(shortDir(t))
	var mu sync.Mutex
	open := 0
	s := &Server{MaxConns: 2, OnConn: func(c *Conn, o bool) {
		mu.Lock()
		defer mu.Unlock()
		if o {
			open++
		} else {
			open--
		}
	}}
	echoServer(t, p, s)
	var cs []*Client
	for i := 0; i < 2; i++ {
		c, err := Dial(p, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Do(context.Background(), "echo", 1, nil); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	c3, err := Dial(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c3.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("third connection not refused")
	}
	cs[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.Conns() != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Conns() != 1 {
		t.Fatalf("conns = %d", s.Conns())
	}
	mu.Lock()
	defer mu.Unlock()
	if open != 1 {
		t.Fatalf("OnConn count %d", open)
	}
}

func TestSocketPathFallback(t *testing.T) {
	short := filepath.Join(os.TempDir(), "x", "run")
	if got := SocketPath(short); got != filepath.Join(short, "service.sock") {
		t.Fatalf("short: %s", got)
	}
	long := filepath.Join(os.TempDir(), strings.Repeat("d", 120), "run")
	got := SocketPath(long)
	if len(got) > maxSocketPath || got == filepath.Join(long, "service.sock") {
		t.Fatalf("long: %s (%d)", got, len(got))
	}
	if SocketPath(long) != got || SocketPath(long+"2") == got {
		t.Fatal("fallback is not a stable function of the run dir")
	}
	// a long run dir still works end to end
	d, err := os.MkdirTemp("", "almlong")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	run := filepath.Join(d, strings.Repeat("l", 110), "run")
	p := SocketPath(run)
	defer os.Remove(p) // the fallback dir is per user: never remove it
	echoServer(t, p, &Server{})
	c, err := Dial(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Do(context.Background(), "echo", 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPermissions(t *testing.T) {
	run := shortDir(t)
	p := SocketPath(run)
	l, err := Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if runtime.GOOS == "windows" {
		checkUserOnlyACL(t, filepath.Dir(p))
		return
	}
	di, _ := os.Stat(filepath.Dir(p))
	si, _ := os.Stat(p)
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 {
		t.Fatalf("dir %v socket %v", di.Mode().Perm(), si.Mode().Perm())
	}
	// a too-open existing directory is tightened
	os.Chmod(filepath.Dir(p), 0o755)
	if err := PrepareDir(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if di, _ := os.Stat(filepath.Dir(p)); di.Mode().Perm() != 0o700 {
		t.Fatalf("not tightened: %v", di.Mode().Perm())
	}
}

func TestSocketPathFallbackIsUnderCacheDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows keeps its temp dir")
	}
	home, err := os.MkdirTemp("/tmp", "almh") // short: macOS's temp dir is too long for a socket under HOME/Library/Caches
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	long := filepath.Join(os.TempDir(), strings.Repeat("d", 120), "run")
	got := SocketPath(long)
	if !strings.HasPrefix(got, home) || strings.HasPrefix(got, "/tmp/alm-") {
		t.Fatalf("fallback %s, want it under the cache dir %s", got, home)
	}
}
