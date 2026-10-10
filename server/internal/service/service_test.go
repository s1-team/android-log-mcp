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
	for _, want := range []string{"service: background service pid", "version 0.4.0", "protocol 1", "root: " + ctrl.Dir} {
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
