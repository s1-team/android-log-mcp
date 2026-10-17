package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
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
//	lingerexit  releases the lock when <ctrl dir>/release appears, then stays
//	            alive for ALM_TEST_LINGER_MS more (the lock is free, the pid is not gone);
//	            with lingerterm a SIGTERM does the same
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
	if strings.Contains(mode, "lingerexit") {
		term := make(chan os.Signal, 1)
		if strings.Contains(mode, "lingerterm") {
			signal.Notify(term, syscall.SIGTERM) // overrides ignoreterm
		}
		for {
			select {
			case <-term:
				goto release
			default:
			}
			if _, err := os.Stat(filepath.Join(ctrl.Dir, "release")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	release:
		store.ReleaseLock(lock)
		ms, _ := strconv.Atoi(os.Getenv("ALM_TEST_LINGER_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return 0, true
	}
	time.Sleep(10 * time.Minute)
	return 0, true
}

type holder struct {
	pid  int
	done chan struct{}
}

// lingerMS is how long a lingerexit holder stays after releasing the lock.
const lingerMS = 1500

func startHolder(t *testing.T, ctrl Control, root, mode string) *holder {
	t.Helper()
	cmd := procattr.Hide(exec.Command(os.Args[0]))
	cmd.Env = append(os.Environ(), "ALM_TEST_HOLDER="+mode, "ALM_TEST_CTRL="+ctrl.Dir, "ALM_TEST_ROOT="+root, "ALM_TEST_LINGER_MS="+strconv.Itoa(lingerMS))
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
	t.Parallel()
	for _, mode := range []string{"silent", "silent,ignoreterm", "nolistener"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", mode)
			c := newRepairClient(ctrl)
			defer c.Close()
			t0 := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
			defer cancel()
			hello, err := c.Connect(ctx)
			if err != nil {
				t.Fatalf("%v (after %s)\nservice log:\n%s", err, time.Since(t0), logTail(ctrl))
			}
			// Nominal 4-7 s (SilentFor, TermWait, KillWait are fixed waits);
			// a -race -parallel stress run needs 3x that (20.25 s seen, #103).
			// Only a hang is a failure (the context above), not elapsed time.
			t.Logf("replaced after %s", time.Since(t0))
			if !h.waitDead(scaled(60 * time.Second)) {
				t.Fatalf("the silent holder is still running\nservice log:\n%s", logTail(ctrl))
			}
			if hello.PID != os.Getpid() {
				t.Fatalf("connected to pid %d, want the new in-process service", hello.PID)
			}
		})
	}
}

func TestForeignHolderIsNotSignalled(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"silent,badexe", "silent,badstart"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", mode)
			c := newRepairClient(ctrl)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
			defer cancel()
			_, err := c.Connect(ctx)
			if err == nil || !strings.Contains(err.Error(), "not stopped automatically") || !strings.Contains(err.Error(), "service stop --force") {
				t.Fatalf("err = %v", err)
			}
			notDeadWithin(t, h, 500*time.Millisecond, "a process that is not this service was signalled")
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
	t.Parallel()
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
			t.Parallel()
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
				notDeadWithin(t, h, 300*time.Millisecond, fmt.Sprintf("force=%v signalled the holder", force))
			}
			// the automatic path refuses too
			c := newRepairClient(ctrl)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
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
	t.Parallel()
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
			ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
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
	t.Logf("all clients connected after %s", time.Since(t0))
	if !h.waitDead(scaled(60 * time.Second)) {
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
	t.Parallel()
	ctrl := testControl(t)
	h := startHolder(t, ctrl, "", "silent")
	now := time.Now()
	b, _ := json.Marshal([]int64{now.Add(-10 * time.Second).UnixNano(), now.Add(-5 * time.Second).UnixNano(), now.UnixNano()})
	os.WriteFile(filepath.Join(ctrl.Run(), "replace.json"), b, 0o600)
	c := newRepairClient(ctrl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
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
	t.Parallel()
	ctrl := testControl(t)
	root := t.TempDir()
	os.MkdirAll(LogDir(root), 0o755)
	os.WriteFile(filepath.Join(LogDir(root), "service.log"), []byte("2026/10/15 service started\n2026/10/15 service stopping: test\n"), 0o644)
	h := startHolder(t, ctrl, root, "silent")
	c := newRepairClient(ctrl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
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
	t.Parallel()
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
	if !h.waitDead(scaled(60*time.Second)) || Running(ctrl) {
		t.Fatal("--force did not stop the holder")
	}
}

func TestSocketWatchRecreatesSocket(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	s, err := Start(Options{Control: ctrl, Version: "0.5.0", Drain: time.Second, SocketWatch: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop("test end") })
	if err := os.Remove(ctrl.Socket()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the socket to be recreated", 60*time.Second, func() bool {
		c, err := Dial(context.Background(), ctrl, "0.5.0", "cli")
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
}

func TestIdentify(t *testing.T) {
	t.Parallel()
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

// pidGone: the holder's pid no longer is the process service.json named.
func pidGone(ctrl Control, st *State) bool {
	alive, ours, _, _ := identify(st)
	return !alive || !ours
}

// The lock is free but the old pid is still exiting: StopHolder returns only
// after the pid is gone (#226 wait-for-exit), so a successor never starts
// beside a process that still holds its directories (Windows).
func TestStopHolderWaitsForPidExit(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	h := startHolder(t, ctrl, "", "lingerexit,ignoreterm,nolistener")
	d := Diagnose(ctrl)
	st := *d.State
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.WriteFile(filepath.Join(ctrl.Dir, "release"), nil, 0o600)
	}()
	t0 := time.Now()
	res, err := StopHolder(ctrl, d, StopOptions{Auto: true, TermWait: scaled(20 * time.Second), KillWait: scaled(5 * time.Second), ExitWait: scaled(20 * time.Second)})
	if err != nil || res.Killed {
		t.Fatalf("StopHolder: %+v %v", res, err)
	}
	if !pidGone(ctrl, &st) {
		t.Fatalf("StopHolder returned after %s with the old pid %d still running", time.Since(t0), h.pid)
	}
	if time.Since(t0) < lingerMS*time.Millisecond/2 {
		t.Fatalf("returned after %s: did not wait for the exit", time.Since(t0))
	}
	if ctrl.ReadState() != nil {
		t.Fatal("service.json not cleaned up")
	}
}

// ExitWait is a bound, not an error: a pid that outlasts it is logged and
// StopHolder still succeeds (the behaviour before the wait existed).
func TestStopHolderExitWaitExpires(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	startHolder(t, ctrl, "", "lingerexit,ignoreterm,nolistener")
	d := Diagnose(ctrl)
	os.WriteFile(filepath.Join(ctrl.Dir, "release"), nil, 0o600)
	t0 := time.Now()
	if _, err := StopHolder(ctrl, d, StopOptions{Auto: true, TermWait: scaled(20 * time.Second), ExitWait: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el > lingerMS*time.Millisecond*3 {
		t.Fatalf("took %s", el)
	}
}

// A reused pid (same pid, other start time) is gone, not waited for.
func TestWaitProcExitReusedPid(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	startHolder(t, ctrl, "", "silent")
	st := *Diagnose(ctrl).State
	t0 := time.Now()
	if waitProcExit(&st, 150*time.Millisecond) {
		t.Fatal("a live holder with its own start time is not gone")
	}
	if time.Since(t0) < 100*time.Millisecond {
		t.Fatal("did not wait")
	}
	st.StartTime++
	t0 = time.Now()
	if !waitProcExit(&st, 10*time.Second) || time.Since(t0) > 2*time.Second {
		t.Fatalf("reused pid was waited for (%s)", time.Since(t0))
	}
	st.StartTime--
	st.PID = 0
	if !waitProcExit(&st, time.Second) {
		t.Fatal("no pid is gone")
	}
}

// Replacing a hung service with real processes: when Connect returns, the
// old pid is gone even though it kept running after releasing the lock.
func TestReplaceWaitsForOldPid(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the graceful stop of a silent holder is SIGTERM; StopHolder's wait is covered above")
	}
	ctrl := testControl(t)
	h := startHolder(t, ctrl, "", "lingerexit,lingerterm,silent")
	st := *Diagnose(ctrl).State
	c := newRepairClient(ctrl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaled(120*time.Second))
	defer cancel()
	if _, err := c.Connect(ctx); err != nil {
		t.Fatalf("%v\nservice log:\n%s", err, logTail(ctrl))
	}
	if !pidGone(ctrl, &st) {
		t.Fatalf("connected while the old holder (pid %d) was still running", h.pid)
	}
}
