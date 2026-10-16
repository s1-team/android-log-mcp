package main

import (
	"archive/zip"
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
	t.Parallel()
	env := isoEnv(t)
	a, b := startMCP(t, env), startMCP(t, env)
	pa, _ := servicePID(t, a.status(t))
	pb, _ := servicePID(t, b.status(t))
	if pa != pb || pa == a.cmd.Process.Pid || pa == b.cmd.Process.Pid {
		t.Fatalf("service pids %d %d (clients %d %d)", pa, pb, a.cmd.Process.Pid, b.cmd.Process.Pid)
	}
	// info messages go to the service log only; stderr stays free of them (#84)
	if lg := serviceLog(env); !strings.Contains(lg, "using the android-log-mcp service pid "+strconv.Itoa(pa)) {
		t.Fatalf("the service log does not name the service:\n%s", lg)
	}
	if strings.Contains(a.err.String()+b.err.String(), "using the android-log-mcp service pid") {
		t.Fatalf("an info message reached stderr:\n%s", a.err.String())
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
	t.Parallel()
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
	t.Parallel()
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
	// A changed version proves the old service was replaced; the pid may
	// legitimately be the same: Windows reuses a freed pid at once (#103).
	if code != 0 || v1 != "0.4.0" || v2 != "0.4.1" || !strings.Contains(serviceLog(env), "older than this MCP or CLI process") || strings.Contains(errOut, "older than this MCP or CLI process") {
		t.Fatalf("0.4.1: %d %d/%s %d/%s\n%s", code, p1, v1, p2, v2, errOut)
	}
	if p2 != p1 { // with a reused pid, p1 names the new service
		waitGone(t, p1)
	}
	out, _, code = call("0.4.0")
	p3, v3 := servicePID(t, out)
	if code != 0 || p3 != p2 || v3 != "0.4.1" || !strings.Contains(out, "code=version_skew") || !strings.Contains(out, "compatible=1") {
		t.Fatalf("old MCP process replaced the newer service or failed: %d %d %s\n%s", code, p3, v3, out)
	}
	out, errOut, code = call("0.4.0", "ALM_TEST_PROTO=0")
	if code == 0 || !strings.Contains(out+errOut, "Upgrade android-log-mcp") {
		t.Fatalf("incompatible protocol: %d\n%s\n%s", code, out, errOut)
	}
	if pid := statePID(t, env); pid != p2 {
		t.Fatalf("service after incompatible client: %d, want %d", pid, p2)
	}
}

// serviceLog is the text of the service log of the isolated environment.
func serviceLog(env []string) string {
	b, _ := os.ReadFile(filepath.Join(controlDirOf(env), "log", "service.log"))
	return string(b)
}

// `--offline <root> report --logs-only` works with no service and no adb and
// makes a zip that holds only diagnostics.
func TestReportLogsOnlyOffline(t *testing.T) {
	t.Parallel()
	env := isoEnv(t)
	root := controlDirOf(env)
	if err := os.MkdirAll(filepath.Join(root, "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "log", "service.log"), []byte("2026/10/15 service started\n"), 0o644)
	out := filepath.Join(t.TempDir(), "r.zip")
	stdout, stderr, code := cliEnv(t, env, "--offline", root, "report", "--logs-only", "-o", out)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
		if strings.HasPrefix(f.Name, "store/") || strings.HasPrefix(f.Name, "devices/") {
			t.Fatalf("logs-only zip contains %s", f.Name)
		}
	}
	for _, want := range []string{"diagnostics/service.log", "diagnostics/diagnose.txt", "manifest.json", "README.txt"} {
		if !got[want] {
			t.Fatalf("zip lacks %s (have %v)", want, got)
		}
	}
}
