package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// isoEnv is an environment with its own user cache dir (so its own data
// root, control dir and service), no adb and no recording wait.
func isoEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	d, err := os.MkdirTemp("", "almc")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"XDG_CACHE_HOME=" + filepath.Join(d, "c"), "HOME=" + d, "USERPROFILE=" + d, "LocalAppData=" + filepath.Join(d, "l"),
		"ANDROID_LOG_MCP_ADB=" + filepath.Join(d, "no-adb"),
	}
	env = append(env, extra...)
	t.Cleanup(func() {
		cliEnv(t, env, "service", "stop")
		if pid := statePID(t, env); pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
		time.Sleep(100 * time.Millisecond)
		os.RemoveAll(d)
	})
	return env
}

func cliEnv(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), "ALM_RUN_MAIN=1"), env...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// statePID is the pid in the env's service.json (0: none).
func statePID(t *testing.T, env []string) int {
	out, _, code := cliEnv(t, env, "service", "status", "--json")
	if code != 0 {
		return 0
	}
	var st struct {
		PID int `json:"pid"`
	}
	json.Unmarshal([]byte(out), &st)
	return st.PID
}

var svcPIDRe = regexp.MustCompile(`service: [a-z -]+(?:\([^)]*\) )?pid (\d+), version (\S+),`)

func servicePID(t *testing.T, text string) (int, string) {
	t.Helper()
	m := svcPIDRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no service line in:\n%s", text)
	}
	pid, _ := strconv.Atoi(m[1])
	return pid, m[2]
}

type session struct {
	cs  *mcp.ClientSession
	cmd *exec.Cmd
	err *syncBuf
}

// syncBuf is a strings.Builder safe for a writer goroutine and readers.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startMCP(t *testing.T, env []string) *session {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), "ALM_RUN_MAIN=1"), env...)
	errb := &syncBuf{}
	cmd.Stderr = errb
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 10 * time.Second}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v\n%s", err, errb.String())
	}
	s := &session{cs: cs, cmd: cmd, err: errb}
	t.Cleanup(func() { s.cs.Close() })
	return s
}

func (s *session) status(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: "logcat_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call: %v\nstderr:\n%s", err, s.err.String())
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(txt, "service: ") {
		t.Fatalf("status without service line (isError=%v):\n%s\nstderr:\n%s", res.IsError, txt, s.err.String())
	}
	return txt
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d still running", pid)
}

// Two MCP clients share one service; when the service is killed, both
// recover on their next call with one new service.
func TestClientsShareServiceAndRecover(t *testing.T) {
	env := isoEnv(t)
	a, b := startMCP(t, env), startMCP(t, env)
	pa, _ := servicePID(t, a.status(t))
	pb, _ := servicePID(t, b.status(t))
	if pa != pb || pa == a.cmd.Process.Pid || pa == b.cmd.Process.Pid {
		t.Fatalf("service pids %d %d (clients %d %d)", pa, pb, a.cmd.Process.Pid, b.cmd.Process.Pid)
	}
	if !strings.Contains(a.err.String()+b.err.String(), "using the android-log-mcp service pid "+strconv.Itoa(pa)) {
		t.Fatalf("stderr does not name the service:\n%s", a.err.String())
	}
	p, _ := os.FindProcess(pa)
	p.Kill()
	waitGone(t, pa)
	na, _ := servicePID(t, a.status(t))
	nb, _ := servicePID(t, b.status(t))
	if na == pa || na != nb {
		t.Fatalf("after kill: %d %d (old %d)", na, nb, pa)
	}
}

// When a detached start is impossible, the first MCP client runs the service
// itself and other clients use it; when it exits, they take over.
func TestEmbeddedFallback(t *testing.T) {
	env := isoEnv(t, "ALM_TEST_NO_SPAWN=1")
	a := startMCP(t, env)
	txt := a.status(t)
	pa, _ := servicePID(t, txt)
	if pa != a.cmd.Process.Pid || !strings.Contains(txt, "in-process service") {
		t.Fatalf("A: service pid %d, client %d:\n%s", pa, a.cmd.Process.Pid, txt)
	}
	b := startMCP(t, env)
	if pb, _ := servicePID(t, b.status(t)); pb != pa {
		t.Fatalf("B uses %d, want A's %d", pb, pa)
	}
	a.cs.Close()
	waitGone(t, pa)
	txt = b.status(t)
	if pb, _ := servicePID(t, txt); pb != b.cmd.Process.Pid {
		t.Fatalf("after A exited, service pid %d, B is %d:\n%s", pb, b.cmd.Process.Pid, txt)
	}
}

// A client replaces only an older service; an older client uses a newer
// service if the protocol is compatible and fails clearly otherwise.
func TestVersionHandoff(t *testing.T) {
	env := isoEnv(t)
	call := func(v string, extra ...string) (string, string, int) {
		return cliEnv(t, append(append(env, "ALM_TEST_VERSION="+v), extra...), "call", "logcat_status")
	}
	out, errOut, code := call("0.4.0")
	if code != 0 {
		t.Fatalf("0.4.0: %d %s %s", code, out, errOut)
	}
	p1, v1 := servicePID(t, out)
	out, errOut, code = call("0.4.1")
	p2, v2 := servicePID(t, out)
	if code != 0 || p2 == p1 || v1 != "0.4.0" || v2 != "0.4.1" || !strings.Contains(errOut, "older than this client") {
		t.Fatalf("0.4.1: %d %d/%s %d/%s\n%s", code, p1, v1, p2, v2, errOut)
	}
	waitGone(t, p1)
	out, _, code = call("0.4.0")
	p3, v3 := servicePID(t, out)
	if code != 0 || p3 != p2 || v3 != "0.4.1" || !strings.Contains(out, "older than the service; compatible") {
		t.Fatalf("old client replaced the newer service or failed: %d %d %s\n%s", code, p3, v3, out)
	}
	out, errOut, code = call("0.4.0", "ALM_TEST_PROTO=0")
	if code == 0 || !strings.Contains(out+errOut, "Upgrade this client") {
		t.Fatalf("incompatible protocol: %d\n%s\n%s", code, out, errOut)
	}
	if pid := statePID(t, env); pid != p2 {
		t.Fatalf("service after incompatible client: %d, want %d", pid, p2)
	}
}
