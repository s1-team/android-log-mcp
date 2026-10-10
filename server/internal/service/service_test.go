package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
)

// testControl is a control dir with a short path (sun_path limits).
func testControl(t *testing.T) Control {
	t.Helper()
	d, err := os.MkdirTemp("", "alms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return Control{Dir: filepath.Join(d, "c")}
}

func startTest(t *testing.T, ctrl Control, version string) *Service {
	t.Helper()
	s, err := Start(Options{Control: ctrl, Version: version, Drain: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop("test end") })
	return s
}

func TestServiceSingleInstanceAndOps(t *testing.T) {
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.4.0")
	if _, err := Start(Options{Control: ctrl, Version: "0.4.0"}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start: %v", err)
	}
	if !Running(ctrl) {
		t.Fatal("lock not held")
	}
	st := ctrl.ReadState()
	if st == nil || st.PID != os.Getpid() || st.Version != "0.4.0" || st.Proto != Proto || st.Root != ctrl.Dir || st.Socket != ctrl.Socket() {
		t.Fatalf("state %+v", st)
	}
	c, err := Dial(context.Background(), ctrl, "0.4.0", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Hello.Version != "0.4.0" || c.Hello.Proto != Proto || len(c.Hello.Tools) != 4 || c.Hello.Instructions == "" || c.Hello.Root != ctrl.Dir {
		t.Fatalf("hello %+v", c.Hello)
	}
	var r CallReply
	if err := c.Do(context.Background(), "call", CallArgs{Name: "logcat_status"}, &r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"service: background service pid", "version 0.4.0", "protocol 2", "root: " + ctrl.Dir} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("status lacks %q:\n%s", want, r.Text)
		}
	}
	var info Info
	if err := c.Do(context.Background(), "status", nil, &info); err != nil || info.Clients != 1 || info.PID != os.Getpid() {
		t.Fatalf("info %+v %v", info, err)
	}
	// ops other than hello/status need a hello first
	raw, err := ipc.Dial(ctrl.Socket(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.Do(context.Background(), "call", CallArgs{Name: "logcat_status"}, nil); err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("call without hello: %v", err)
	}
	if err := c.Do(context.Background(), "stop", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("not stopped")
	}
	if Running(ctrl) || ctrl.ReadState() != nil {
		t.Fatal("lock or state left after stop")
	}
	if _, err := os.Stat(ctrl.Socket()); !os.IsNotExist(err) {
		t.Fatalf("socket left: %v", err)
	}
	// a new instance can start again (also over a stale socket file)
	os.WriteFile(ctrl.Socket(), nil, 0o600)
	startTest(t, ctrl, "0.4.0")
	if _, err := Dial(context.Background(), ctrl, "0.4.0", "mcp"); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestHandoffOnlyToNewer(t *testing.T) {
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.4.0")
	for _, v := range []string{"0.4.0", "0.3.9", "0.4.0-rc.1"} {
		c, err := Dial(context.Background(), ctrl, v, "mcp")
		if err != nil {
			t.Fatal(err)
		}
		err = c.Do(context.Background(), "handoff", nil, nil)
		c.Close()
		if err == nil || !strings.Contains(err.Error(), "not newer") {
			t.Fatalf("handoff from %s: %v", v, err)
		}
	}
	c, err := Dial(context.Background(), ctrl, "0.4.1", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Do(context.Background(), "handoff", nil, nil); err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	if !strings.Contains(s.Reason(), "0.4.1") {
		t.Fatalf("reason %q", s.Reason())
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.4.0", 0}, {"0.4.0", "0.3.9", 1}, {"0.3.9", "0.4.0", -1}, {"1.0.0", "0.99.99", 1},
		{"0.4.0", "0.4.0-rc.1", 1}, {"0.4.0-rc.2", "0.4.0-rc.10", -1}, {"0.4.0-dev", "0.4.0-dev+abc", 0},
		{"v0.4.0", "0.4.0", 0}, {"0.4.0-1", "0.4.0-alpha", -1}, {"garbage", "0.4.0", 0}, {"", "0.4.0", 0},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if !ProtoCompatible(1, 1, 2) || ProtoCompatible(1, 2, 3) || ProtoCompatible(3, 1, 2) {
		t.Fatal("ProtoCompatible")
	}
}

func TestLogFileRotation(t *testing.T) {
	root := t.TempDir()
	l, err := OpenLog(root)
	if err != nil {
		t.Fatal(err)
	}
	l.Max = 1000
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 55; i++ {
		l.Write(line)
	}
	l.Close()
	a, _ := os.Stat(filepath.Join(root, "log", "service.log"))
	b, _ := os.Stat(filepath.Join(root, "log", "service.log.1"))
	if a == nil || b == nil || a.Size() > 1000 || b.Size() > 1000 {
		t.Fatalf("rotation: %v %v", a, b)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "log"))
	if len(ents) != 2 {
		t.Fatalf("log files: %d", len(ents))
	}
}

func TestLingerFor(t *testing.T) {
	for _, c := range []struct {
		span time.Duration
		env  string
		want time.Duration
	}{
		{0, "", 10 * time.Minute},
		{3 * time.Minute, "", 10 * time.Minute},
		{8 * time.Minute, "", 16 * time.Minute},
		{40 * time.Minute, "", time.Hour},
		{0, "0", 0},
		{0, "2s", 2 * time.Second},
		{0, "90", 90 * time.Second},
		{0, "forever", -1},
		{0, "bogus", 10 * time.Minute},
	} {
		if got, _ := lingerFor(c.span, c.env); got != c.want {
			t.Errorf("lingerFor(%s, %q) = %s, want %s", c.span, c.env, got, c.want)
		}
	}
	if _, how := lingerFor(0, ""); !strings.Contains(how, "default") || !strings.Contains(how, "10m0s") {
		t.Errorf("description %q", how)
	}
}

// shortExit makes the service exit quickly when unused (tests).
func shortExit(t *testing.T, grace time.Duration) {
	t.Helper()
	og, ot := ExitGrace, exitTick
	ExitGrace, exitTick = grace, 20*time.Millisecond
	t.Cleanup(func() { ExitGrace, exitTick = og, ot })
}

// Issue #11 §6: the service never exits while clients are connected (an
// idle connected service runs no adb) and exits ExitGrace after there are
// no clients, no pin and no recorded device; ANDROID_LOG_MCP_LINGER only
// concerns devices.
func TestServiceLifetime(t *testing.T) {
	t.Setenv(LingerEnv, "0")
	shortExit(t, 500*time.Millisecond)
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.5.0")
	time.Sleep(200 * time.Millisecond)
	select {
	case <-s.Done():
		t.Fatal("stopped before the first client could connect")
	default:
	}
	c, err := Dial(context.Background(), ctrl, "0.5.0", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Hello.Recording, "idle") {
		t.Errorf("hello recording %q", c.Hello.Recording)
	}
	time.Sleep(time.Second) // longer than the grace: a client is connected
	select {
	case <-s.Done():
		t.Fatal("stopped while a client was connected")
	default:
	}
	var info Info
	c.Do(context.Background(), "status", nil, &info)
	if !strings.Contains(info.Recording, "idle (starts on the first log tool call)") || !strings.Contains(info.Lifetime, "staying: 1 client") {
		t.Fatalf("info %+v", info)
	}
	if !strings.Contains(info.Text(), "recording: idle") {
		t.Fatalf("text %s", info.Text())
	}
	c.Close()
	select {
	case <-s.Done():
		if !strings.Contains(s.Reason(), "no client, no pin") {
			t.Fatalf("reason %q", s.Reason())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop after the last client")
	}
	// and without any client it stops after the grace
	s2 := startTest(t, ctrl, "0.5.0")
	select {
	case <-s2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no stop without clients")
	}
}

// An active pin keeps the service alive with no clients; once it ends the
// service exits.
func TestPinKeepsServiceAlive(t *testing.T) {
	shortExit(t, 300*time.Millisecond)
	ctrl := testControl(t)
	if err := os.MkdirAll(ctrl.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	pf := &PinFile{Pins: map[string]*Pin{"dev1": {Deadline: time.Now().Add(1500 * time.Millisecond), SetAt: time.Now(), SetBy: "test"}}}
	if err := WritePins(ctrl, pf); err != nil {
		t.Fatal(err)
	}
	s := startTest(t, ctrl, "0.5.0")
	time.Sleep(time.Second)
	select {
	case <-s.Done():
		t.Fatal("stopped although a pin is active")
	default:
	}
	if txt := s.Info().Text(); !strings.Contains(txt, "dev1: pinned until") {
		t.Errorf("status text:\n%s", txt)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop after the pin ended")
	}
}
