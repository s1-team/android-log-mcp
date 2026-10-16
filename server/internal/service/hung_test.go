package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/procattr"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// holderMain is this test binary re-executed as a process that holds the
// service lock the way a hung service does: modes (comma separated)
//
//	silent      listens on the socket but never accepts (hello is never answered)
//	nolistener  no socket at all
//	ignoreterm  ignores SIGTERM (so the graceful stop does not work)
//	badexe      service.json names another executable
//	badstart    service.json has another start time
//	embedded    service.json says the service runs inside an MCP process
//
// It writes service.json like a service and prints "ready".
func holderMain() (int, bool) {
	mode := os.Getenv("ALM_TEST_HOLDER")
	if mode == "" {
		return 0, false
	}
	ctrl := Control{Dir: os.Getenv("ALM_TEST_CTRL")}
	if err := ipc.PrepareDir(ctrl.Run()); err != nil {
		return 4, true
	}
	lock, ok := store.TryLock(ctrl.LockPath())
	if !ok {
		return 3, true
	}
	_ = lock
	if strings.Contains(mode, "ignoreterm") {
		signal.Ignore(syscall.SIGTERM)
	}
	if strings.Contains(mode, "silent") {
		l, err := ipc.Listen(ctrl.Socket())
		if err != nil {
			return 5, true
		}
		_ = l // never Accept: connects succeed (backlog), nothing answers
	}
	start, exe, _ := inspectProc(os.Getpid())
	if strings.Contains(mode, "badexe") {
		exe = filepath.Join(os.TempDir(), "some-other-binary")
	}
	if strings.Contains(mode, "badstart") {
		start++
	}
	embedded := strings.Contains(mode, "embedded")
	st := State{Embedded: embedded, PID: os.Getpid(), Socket: ctrl.Socket(), Version: "0.0.1", Proto: Proto, Root: os.Getenv("ALM_TEST_ROOT"), Exe: exe, StartTime: start, Started: time.Now().UTC()}
	b, _ := json.Marshal(st)
	if err := writeFileAtomic(ctrl.StatePath(), b, 0o600); err != nil {
		return 6, true
	}
	os.Stdout.WriteString("ready\n")
	time.Sleep(10 * time.Minute)
	return 0, true
}

type holder struct {
	pid  int
	done chan struct{}
}

func startHolder(t *testing.T, ctrl Control, root, mode string) *holder {
	t.Helper()
	cmd := procattr.Hide(exec.Command(os.Args[0]))
	cmd.Env = append(os.Environ(), "ALM_TEST_HOLDER="+mode, "ALM_TEST_CTRL="+ctrl.Dir, "ALM_TEST_ROOT="+root)
	var out bytes.Buffer
	pr, pw, _ := os.Pipe()
	cmd.Stdout, cmd.Stderr = pw, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	h := &holder{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { cmd.Wait(); close(h.done) }() // reaps it
	t.Cleanup(func() { cmd.Process.Kill(); <-h.done })
	ready := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(pr).ReadString('\n'); ready <- l }()
	select {
	case l := <-ready:
		if !strings.HasPrefix(l, "ready") {
			t.Fatalf("holder (%s) did not start: %q %s", mode, l, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("holder (%s) did not start: %s", mode, out.String())
	}
	return h
}

func (h *holder) dead() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// waitDead reports whether the holder has been reaped within d. The service
// kills the holder and returns as soon as the process is gone; the reaper
// goroutine of startHolder (cmd.Wait) may close done a moment later, more so
// under CPU load, so a test that expects the holder dead waits for it with a
// deadline instead of a single non-blocking look (#103).
func (h *holder) waitDead(d time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(d):
		return false
	}
}

func fastRepair() RepairTiming {
	return RepairTiming{SilentFor: time.Second, StoppingWait: 1500 * time.Millisecond, TermWait: 2 * time.Second, KillWait: 5 * time.Second, WaiterWait: 40 * time.Second, HelloWait: 300 * time.Millisecond}
}

func newRepairClient(ctrl Control) *Client {
	return NewClient(ClientOptions{Control: ctrl, Version: "0.5.0", Role: "mcp", StartTimeout: 3 * time.Second, NoDetach: true, Repair: fastRepair()})
}

// logTail is the service log (the client's repair messages go there) for a
// failure message: an unexplained slow replacement (#103) shows which step
// ate the time.
func logTail(ctrl Control) string {
	root, err := ConfiguredRoot(ctrl)
	if err != nil {
		return "(no service log: " + err.Error() + ")"
	}
	b, err := os.ReadFile(filepath.Join(LogDir(root), "service.log"))
	if err != nil {
		return "(no service log: " + err.Error() + ")"
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}

func TestSilentHolderIsReplaced(t *testing.T) {
	for _, mode := range []string{"silent", "silent,ignoreterm", "nolistener"} {
		t.Run(mode, func(t *testing.T) {
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", mode)
			c := newRepairClient(ctrl)
			defer c.Close()
			t0 := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			hello, err := c.Connect(ctx)
			if err != nil {
				t.Fatalf("%v (after %s)\nservice log:\n%s", err, time.Since(t0), logTail(ctrl))
			}
			// Nominal 4-7 s (SilentFor, TermWait, KillWait are fixed waits);
			// a -race -parallel stress run needs 3x that (20.25 s seen, #103).
			// Only a hang is a failure here, so the bound is far above it
			// and below the 40 s context.
			if el := time.Since(t0); el > 30*time.Second {
				t.Fatalf("took %s\nservice log:\n%s", el, logTail(ctrl))
			}
			if !h.waitDead(10 * time.Second) {
				t.Fatalf("the silent holder is still running\nservice log:\n%s", logTail(ctrl))
			}
			if hello.PID != os.Getpid() {
				t.Fatalf("connected to pid %d, want the new in-process service", hello.PID)
			}
		})
	}
}

func TestForeignHolderIsNotSignalled(t *testing.T) {
	for _, mode := range []string{"silent,badexe", "silent,badstart"} {
		t.Run(mode, func(t *testing.T) {
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", mode)
			c := newRepairClient(ctrl)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := c.Connect(ctx)
			if err == nil || !strings.Contains(err.Error(), "not stopped automatically") || !strings.Contains(err.Error(), "service stop --force") {
				t.Fatalf("err = %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if h.dead() {
				t.Fatal("a process that is not this service was signalled")
			}
			// service stop --force refuses it too
			var out, errb bytes.Buffer
			if code := RunStop(ctrl, "0.5.0", true, &out, &errb); code != 1 || !strings.Contains(errb.String(), "not recognised") {
				t.Fatalf("stop --force: %d %q %q", code, out.String(), errb.String())
			}
			if h.dead() {
				t.Fatal("--force signalled a process that is not this service")
			}
		})
	}
}

// Neither the automatic replacement nor service stop --force may signal a
// holder that is not provably this service: a reused pid (other image or other
// start time) or an in-process service (its pid is an MCP process).
func TestStopRefusesReusedPidAndEmbedded(t *testing.T) {
	cases := []struct{ mode, msg string }{
		{"silent,badexe", "not recognised"},
		{"silent,badstart", "not recognised"},
		{"silent,badexe,ignoreterm", "not recognised"},
		{"silent,badstart,ignoreterm", "not recognised"},
		{"silent,embedded", "inside an MCP process"},
		{"silent,embedded,ignoreterm", "inside an MCP process"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", tc.mode)
			for _, force := range []bool{false, true} {
				var out, errb bytes.Buffer
				code := RunStop(ctrl, "0.5.0", force, &out, &errb)
				if code != 1 {
					t.Fatalf("force=%v: exit %d %q %q", force, code, out.String(), errb.String())
				}
				if force && !strings.Contains(errb.String(), tc.msg) {
					t.Fatalf("force: message %q lacks %q", errb.String(), tc.msg)
				}
				time.Sleep(200 * time.Millisecond)
				if h.dead() {
					t.Fatalf("force=%v signalled the holder", force)
				}
			}
			// the automatic path refuses too
			c := newRepairClient(ctrl)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := c.Connect(ctx); err == nil {
				t.Fatal("Connect succeeded against a holder that must be refused")
			}
			if h.dead() {
				t.Fatal("automatic replacement signalled the holder")
			}
			if !Running(ctrl) || ctrl.ReadState() == nil {
				t.Fatal("the holder's lock or service.json was removed")
			}
		})
	}
}

func TestConcurrentClientsShareOneRepair(t *testing.T) {
	ctrl := testControl(t)
	h := startHolder(t, ctrl, "", "silent")
	const n = 3
	var wg sync.WaitGroup
	pids := make([]int, n)
	errs := make([]error, n)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := newRepairClient(ctrl)
			t.Cleanup(c.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			hello, err := c.Connect(ctx)
			pids[i], errs[i] = hello.PID, err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("client %d: %v", i, errs[i])
		}
		if pids[i] != os.Getpid() {
			t.Fatalf("client %d connected to pid %d", i, pids[i])
		}
	}
	if el := time.Since(t0); el > 25*time.Second {
		t.Fatalf("took %s", el)
	}
	if !h.waitDead(10 * time.Second) {
		t.Fatal("holder still running")
	}
	b, _ := os.ReadFile(filepath.Join(ctrl.Run(), "replace.json"))
	var ts []int64
	json.Unmarshal(b, &ts)
	if len(ts) != 1 {
		t.Fatalf("replacements recorded: %d (%s), want 1", len(ts), b)
	}
}

func TestReplaceRateLimit(t *testing.T) {
	ctrl := testControl(t)
	h := startHolder(t, ctrl, "", "silent")
	now := time.Now()
	b, _ := json.Marshal([]int64{now.Add(-10 * time.Second).UnixNano(), now.Add(-5 * time.Second).UnixNano(), now.UnixNano()})
	os.WriteFile(filepath.Join(ctrl.Run(), "replace.json"), b, 0o600)
	c := newRepairClient(ctrl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "3 times within a minute") {
		t.Fatalf("err = %v", err)
	}
	if h.dead() {
		t.Fatal("replaced despite the rate limit")
	}
}

func TestStoppingServiceIsWaitedFor(t *testing.T) {
	ctrl := testControl(t)
	root := t.TempDir()
	os.MkdirAll(LogDir(root), 0o755)
	os.WriteFile(filepath.Join(LogDir(root), "service.log"), []byte("2026/10/15 service started\n2026/10/15 service stopping: test\n"), 0o644)
	h := startHolder(t, ctrl, root, "silent")
	c := newRepairClient(ctrl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("err = %v", err)
	}
	if h.dead() {
		t.Fatal("a service that is shutting down was killed")
	}
}

func TestStopForce(t *testing.T) {
	ctrl := testControl(t)
	var out, errb bytes.Buffer
	if code := RunStop(ctrl, "0.5.0", false, &out, &errb); code != 0 || !strings.Contains(out.String(), "not running") {
		t.Fatalf("no service: %d %q %q", code, out.String(), errb.String())
	}
	h := startHolder(t, ctrl, "", "silent")
	out.Reset()
	errb.Reset()
	if code := RunStop(ctrl, "0.5.0", false, &out, &errb); code != 1 || !strings.Contains(errb.String(), "service stop --force") || strings.Contains(out.String(), "not running") {
		t.Fatalf("hung, no --force: %d %q %q", code, out.String(), errb.String())
	}
	if h.dead() {
		t.Fatal("stopped without --force")
	}
	out.Reset()
	errb.Reset()
	if code := RunStop(ctrl, "0.5.0", true, &out, &errb); code != 0 {
		t.Fatalf("--force: %d %q %q", code, out.String(), errb.String())
	}
	if !h.waitDead(10*time.Second) || Running(ctrl) {
		t.Fatal("--force did not stop the holder")
	}
}

func TestSocketWatchRecreatesSocket(t *testing.T) {
	ctrl := testControl(t)
	s, err := Start(Options{Control: ctrl, Version: "0.5.0", Drain: time.Second, SocketWatch: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop("test end") })
	if err := os.Remove(ctrl.Socket()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := Dial(context.Background(), ctrl, "0.5.0", "cli")
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket was not recreated: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestIdentify(t *testing.T) {
	start, exe, ok := inspectProc(os.Getpid())
	if !ok || exe == "" {
		t.Skipf("no process inspection on this OS (ok=%v exe=%q)", ok, exe)
	}
	self := &State{PID: os.Getpid(), Exe: exe, StartTime: start}
	if alive, ours, strong, why := identify(self); !alive || !ours || !strong {
		t.Fatalf("self: %v %v %v %s", alive, ours, strong, why)
	}
	if _, ours, _, _ := identify(&State{PID: os.Getpid(), Exe: exe, StartTime: start + 1}); ours {
		t.Fatal("a different start time must not match")
	}
	if _, ours, _, _ := identify(&State{PID: os.Getpid(), Exe: filepath.Join(os.TempDir(), "other"), StartTime: start}); ours {
		t.Fatal("a different image must not match")
	}
	if _, ours, strong, _ := identify(&State{PID: os.Getpid(), Exe: exe}); !ours || strong {
		t.Fatal("no recorded start time: ours by image only, never strong")
	}
	if alive, _, _, _ := identify(&State{PID: 1 << 30, Exe: exe}); alive {
		t.Fatal("a missing pid is not alive")
	}
}
