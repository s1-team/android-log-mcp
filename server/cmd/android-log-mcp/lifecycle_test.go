package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeADBEnv installs the fake adb and returns env entries for it plus its
// state directory.
func fakeADBEnv(t *testing.T) ([]string, string) {
	t.Helper()
	d := t.TempDir()
	name := "adb"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(d, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(dst, src)
	dst.Close()
	state := filepath.Join(d, "state")
	os.MkdirAll(state, 0o755)
	t.Cleanup(func() {
		if pid := readPID(state, "server.pid"); pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				waitGoneQuiet(pid)
			}
		}
	})
	return []string{"ANDROID_LOG_MCP_ADB=" + filepath.Join(d, name), "ALM_FAKE_ADB_DIR=" + state}, state
}

func readPID(dir, name string) int {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func waitFile(t *testing.T, dir, name string) int {
	t.Helper()
	var pid int
	waitUntil(t, name+" to be written", 60*time.Second, func() bool { pid = readPID(dir, name); return pid > 0 })
	return pid
}

func waitGoneQuiet(pid int) bool {
	deadline := time.Now().Add(scaled(60 * time.Second))
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// The adb server is shared with other tools: stopping or killing the
// service ends its own adb children but never the server.
func TestADBServerSurvivesService(t *testing.T) {
	heavyParallel(t)
	adbEnv, state := fakeADBEnv(t)
	env := isoEnv(t, adbEnv...)
	for _, how := range []string{"stop", "kill"} {
		os.Remove(filepath.Join(state, "track.pid"))
		out, errOut, code := cliEnv(t, env, "call", "logcat_status")
		if code != 0 {
			t.Fatalf("%s: call: %d %s %s", how, code, out, errOut)
		}
		svc, _ := servicePID(t, out)
		server := waitFile(t, state, "server.pid")
		track := waitFile(t, state, "track.pid")
		if how == "stop" {
			if _, e, c := cliEnv(t, env, "service", "stop"); c != 0 {
				t.Fatalf("stop: %d %s", c, e)
			}
		} else {
			p, _ := os.FindProcess(svc)
			p.Kill()
		}
		waitGone(t, svc)
		if !waitGoneQuiet(track) {
			t.Fatalf("%s: adb child (track-devices, pid %d) outlived the service", how, track)
		}
		if !processAlive(server) {
			t.Fatalf("%s: the adb server (pid %d) was killed with the service", how, server)
		}
	}
}

// The service stays while a client is connected (idle: no adb) and exits
// ExitGrace (shortened here) after the last client left with no pin and no
// recorded device; the per-device linger default is the planned 10 min
// (2 × buffer span, ≤ 1 h).
func TestServiceExitsAfterLastClient(t *testing.T) {
	heavyParallel(t)
	env := isoEnv(t)
	out, _, code := cliEnv(t, env, "call", "logcat_status")
	if code != 0 || !strings.Contains(out, "linger: per device, after its last log tool call: max(10m0s, 2 × the device's log buffer span), at most 1h0m0s (default") ||
		strings.Contains(out, "not measured") || !strings.Contains(out, "recording: idle (starts on the first log tool call)") {
		t.Fatalf("status:\n%s", out)
	}
	cliEnv(t, env, "service", "stop")

	env = isoEnv(t, "ALM_TEST_EXIT_GRACE=1s", "ANDROID_LOG_MCP_LINGER=0")
	s := startMCP(t, env)
	pid, _ := servicePID(t, s.status(t))
	// longer than the 1s grace while the client is connected (a negative
	// observation: it ends at once if the service exits)
	select {
	case <-exited(pid):
		t.Fatalf("service stopped while an MCP client was connected\n%s", dumpDiags(t))
	case <-time.After(2500 * time.Millisecond):
	}
	s.cs.Close()
	waitGone(t, pid)
	log, _ := os.ReadFile(filepath.Join(controlDirOf(env), "log", "service.log"))
	if !strings.Contains(string(log), "no MCP or CLI process, no pin and no recorded device for 1s") {
		t.Fatalf("service log:\n%s", log)
	}
}

// controlDirOf is the control dir (= default data root) of an isoEnv.
func controlDirOf(env []string) string {
	get := func(k string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(env[i], k+"="); ok {
				return v
			}
		}
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(get("LocalAppData"), "android-log-mcp")
	case "darwin":
		return filepath.Join(get("HOME"), "Library", "Caches", "android-log-mcp")
	}
	return filepath.Join(get("XDG_CACHE_HOME"), "android-log-mcp")
}

// exited closes when pid is no longer running (polled; the goroutine ends
// with the test binary, bounded by the process lifetime check).
func exited(pid int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for processAlive(pid) {
			time.Sleep(20 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}
