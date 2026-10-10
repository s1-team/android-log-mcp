package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

// cli runs this binary's main() with args (see TestMain).
func cli(t *testing.T, env []string, args ...string) (string, string, int) {
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

func TestRemovedHomeIsAnError(t *testing.T) {
	_, errOut, code := cli(t, []string{"ANDROID_LOG_MCP_HOME=/tmp/x"}, "call", "logcat_status")
	if code != 2 || !strings.Contains(errOut, "ANDROID_LOG_MCP_HOME") || !strings.Contains(errOut, "set-root") {
		t.Fatalf("env: code %d stderr %q", code, errOut)
	}
	_, errOut, code = cli(t, nil, "--home", "/tmp/x")
	if code != 2 || !strings.Contains(errOut, "removed") || !strings.Contains(errOut, "set-root") {
		t.Fatalf("--home: code %d stderr %q", code, errOut)
	}
}

func TestSetRootCLI(t *testing.T) {
	os.Remove(layout.ConfigPath())
	t.Cleanup(func() { os.Remove(layout.ConfigPath()) })
	target := filepath.Join(t.TempDir(), "new")
	for _, args := range [][]string{{"set-root", target}, {"set-root", target, "--keep-old", "--delete-old"}, {"set-root", "--keep-old"}} {
		_, errOut, code := cli(t, nil, args...)
		if code == 0 || !strings.Contains(errOut, "--keep-old") || !strings.Contains(errOut, "--delete-old") {
			t.Fatalf("%v: code %d stderr %q", args, code, errOut)
		}
	}
	if _, err := os.Stat(layout.ConfigPath()); err == nil {
		t.Fatal("config written by an invalid call")
	}
	out, _, code := cli(t, nil, "root")
	if code != 0 || strings.TrimSpace(out) != layout.DefaultDir() {
		t.Fatalf("root: %d %q", code, out)
	}
	out, errOut, code := cli(t, nil, "set-root", target, "--keep-old", "--json")
	var res struct {
		NewRoot string `json:"new_root"`
		Outcome string `json:"outcome"`
		Old     string `json:"old"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Outcome != "switched" || res.Old != "keep" {
		t.Fatalf("set-root: %d %q %q", code, out, errOut)
	}
	out, _, _ = cli(t, nil, "root", "--json")
	var cur struct{ Root, Source string }
	if json.Unmarshal([]byte(out), &cur) != nil || cur.Source != "config" || cur.Root != res.NewRoot {
		t.Fatalf("root after set-root: %s", out)
	}
	_, errOut, code = cli(t, nil, "set-root", filepath.Join(res.NewRoot, "inside"), "--keep-old")
	if code != 2 || !strings.Contains(errOut, "inside the current root") {
		t.Fatalf("inside: %d %q", code, errOut)
	}
}
