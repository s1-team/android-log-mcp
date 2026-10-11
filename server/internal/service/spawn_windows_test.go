//go:build windows

package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

const probeRun = "^TestDetachedServiceHasNoVisibleConsole$"

func consoleProbe() (int, bool) {
	out := os.Getenv("ALM_CONSOLE_PROBE")
	if out == "" {
		return 0, false
	}
	return procattr.RunConsoleProbe(out, os.Getenv("ALM_CONSOLE_PROBE_CHILD") != "", probeRun), true
}

// TestDetachedServiceHasNoVisibleConsole starts a helper exactly the way a
// client starts the service (client.spawn: procattr.Hide + detach). The
// helper and a grandchild it starts with a plain exec.Command (like any adb
// call the service makes) must have no visible console window: 0.4.0 used
// DETACHED_PROCESS, so every adb child of the service opened a new window.
func TestDetachedServiceHasNoVisibleConsole(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "probe.txt")
	cmd := procattr.Hide(exec.Command(os.Args[0], "-test.run="+probeRun))
	cmd.Env = append(os.Environ(), "ALM_CONSOLE_PROBE="+out, "ALM_CONSOLE_PROBE_CHILD=1")
	if err := detach(cmd); err != nil {
		t.Fatal(err)
	}
	f := cmd.SysProcAttr.CreationFlags
	if f&windows.DETACHED_PROCESS != 0 || f&windows.CREATE_NO_WINDOW == 0 || f&windows.CREATE_NEW_PROCESS_GROUP == 0 || !cmd.SysProcAttr.HideWindow {
		t.Errorf("service creation flags %#x (HideWindow %v): want CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP, no DETACHED_PROCESS", f, cmd.SysProcAttr.HideWindow)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe:\n%s", b)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want reports from the service and its child, got %q", lines)
	}
	for _, l := range lines {
		if strings.HasSuffix(l, " visible=true") {
			t.Errorf("visible console window: %s", l)
		}
	}
}
