package device

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #24: stopping a device or closing the manager waits for its
// background work and the adb processes it started.

// closeManager is a lazy manager whose test closes it itself.
func closeManager(t *testing.T) (*Manager, context.CancelFunc) {
	t.Helper()
	m := newTestManager(t, t.TempDir(), store.Config{})
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	m.Linger = func(*Device) time.Duration { return time.Minute }
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(cancel)
	return m, cancel
}

// A side-work adb command hangs, and its grandchild holds the output pipe
// open: Close returns only after every fake adb process exited (so the
// temp dir cleanup of the fake's directory succeeds).
func TestCloseWaitsForSideWorkAdb(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	if err := e.SetHang("FAKEA", "pm list packages"); err != nil {
		t.Fatal(err)
	}
	m, cancel := closeManager(t)
	startDevice(t, m, "FAKEA")
	waitFor(t, "the hanging side work", 30*time.Second, e.Hanging)
	t0 := time.Now()
	cancel()
	m.Close()
	took := time.Since(t0)
	// Strict: only a killed process that is already exiting gets a short
	// grace to become a zombie (#61); anything else still running fails.
	if pids := e.RunningSettled(200 * time.Millisecond); len(pids) > 0 {
		t.Fatalf("Close returned after %s with fake adb processes still running: %v", took, pids)
	}
	if took > testTiming(t).BgStopWait.Get()+3*time.Second {
		t.Errorf("Close took %s", took)
	}
}

// Background work that ignores the cancellation is logged after
// bgStopWait, and its adb processes are killed with their trees.
func TestStuckBackgroundWorkIsKilled(t *testing.T) {
	// serial: swaps the package-wide log output
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	if err := e.SetHang("FAKEA", "STUCKCMD"); err != nil {
		t.Fatal(err)
	}
	testTiming(t).BgStopWait.Set(300 * time.Millisecond)
	var buf syncBuf
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	m, cancel := closeManager(t)
	d := startDevice(t, m, "FAKEA")
	d.bg.spawn("stuck", func() {
		// not the session context: only the kill after bgStopWait ends it
		d.adb.Shell(withChildren(context.Background(), &d.kids), d.Addr(), time.Minute, "STUCKCMD")
	})
	waitFor(t, "the stuck command", 30*time.Second, e.Hanging)
	cancel()
	m.Close()
	out := buf.String()
	if !strings.Contains(out, "FAKEA: background work did not stop within 300ms: stuck; killed its adb processes: adb -s FAKEA shell STUCKCMD") {
		t.Errorf("log:\n%s", out)
	}
	waitFor(t, "the killed processes to exit", 5*time.Second, func() bool { return len(e.Running()) == 0 })
	if names := d.bg.wait(5 * time.Second); len(names) > 0 {
		t.Errorf("still running: %v", names)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Stderr.Write(p)
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The tree walk skips adb processes (servers) and their subtrees.
func TestDescendantsSkipADBServer(t *testing.T) {
	t.Parallel()
	procs := []procEntry{
		{pid: 10, ppid: 1, name: "adb"},       // our adb client (root)
		{pid: 11, ppid: 10, name: "sh"},       // a grandchild
		{pid: 12, ppid: 11, name: "sleep"},    // its child
		{pid: 13, ppid: 10, name: "adb.exe"},  // a server the client started
		{pid: 14, ppid: 13, name: "helper"},   // below the server
		{pid: 15, ppid: 1, name: "unrelated"}, //
	}
	got := descendants(10, procs, nil)
	if !slices.Equal(got, []int{11, 12}) {
		t.Fatalf("descendants = %v", got)
	}
	if !isADBName(filepath.Join("C:", "x", "ADB.EXE")) || isADBName("adbhang") {
		t.Fatal("isADBName")
	}
}

// killTree on the real process table: the fake's hang grandchild dies
// with it.
func TestKillTreeKillsGrandchild(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 1, "FAKEA")
	if err := e.SetHang("FAKEA", "STUCKCMD"); err != nil {
		t.Fatal(err)
	}
	a := &ADB{Path: fakeADB(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Shell(ctx, "FAKEA", time.Minute, "STUCKCMD")
		done <- err
	}()
	waitFor(t, "the grandchild", 30*time.Second, e.Hanging)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled adb call hung")
	}
	waitFor(t, "fake processes to exit", 5*time.Second, func() bool { return len(e.Running()) == 0 })
	_ = fakeadb.HelperLateFile
}
