//go:build windows

package procattr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHideSetsNoWindow(t *testing.T) {
	c := Hide(exec.Command("cmd.exe", "/c", "exit"))
	a := c.SysProcAttr
	if a == nil || !a.HideWindow || a.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("SysProcAttr = %+v, want HideWindow and CREATE_NO_WINDOW", a)
	}
	// existing flags are kept
	c = exec.Command("cmd.exe")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	Hide(c)
	if f := c.SysProcAttr.CreationFlags; f&windows.CREATE_NEW_PROCESS_GROUP == 0 || f&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("flags = %#x, want CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW", f)
	}
}

// TestChildHasNoVisibleConsole starts this test binary through Hide; the
// child reports its console window and starts a grandchild with a plain
// exec.Command (no flags), which must inherit the hidden console rather
// than open a new window.
func TestChildHasNoVisibleConsole(t *testing.T) {
	if os.Getenv(probeEnv) != "" {
		return // the probe itself runs from TestMain
	}
	out := filepath.Join(t.TempDir(), "probe.txt")
	c := Hide(exec.Command(os.Args[0], "-test.run=^TestChildHasNoVisibleConsole$"))
	c.Env = append(os.Environ(), probeEnv+"="+out, probeChildEnv+"=1")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe: %s", b)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasSuffix(l, " visible=true") {
			t.Errorf("a child process has a visible console window: %s", l)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(string(b)), "\n")); n != 2 {
		t.Errorf("want reports from the child and the grandchild, got %d lines", n)
	}
}

const (
	probeEnv      = "ALM_CONSOLE_PROBE"
	probeChildEnv = "ALM_CONSOLE_PROBE_CHILD"
)

func TestMain(m *testing.M) {
	if out := os.Getenv(probeEnv); out != "" {
		os.Exit(RunConsoleProbe(out, os.Getenv(probeChildEnv) != "", "^TestChildHasNoVisibleConsole$"))
	}
	os.Exit(m.Run())
}
