package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{[]string{"ANDROID_LOG_MCP_HOME=/tmp/x"}, []string{"call", "logcat_status"}},
		{nil, []string{"--home", "/tmp/x"}},
		{nil, []string{"--home=/tmp/x", "report"}},
	} {
		_, errOut, code := cli(t, c.env, c.args...)
		if code != 2 || !strings.Contains(errOut, "removed") || !strings.Contains(errOut, "fixed") || !strings.Contains(errOut, "issues/3") {
			t.Fatalf("%v %v: code %d stderr %q", c.env, c.args, code, errOut)
		}
	}
}

func TestRootCommand(t *testing.T) {
	out, _, code := cli(t, nil, "root")
	if code != 0 || strings.TrimSpace(out) != layout.DefaultDir() {
		t.Fatalf("root: %d %q", code, out)
	}
	out, _, code = cli(t, nil, "root", "--json")
	var r struct{ Root, Control, Config, Socket string }
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Root != layout.DefaultDir() || r.Control != layout.DefaultDir() || r.Socket == "" {
		t.Fatalf("root --json: %d %q", code, out)
	}
	// set-root is not part of 0.4.0 (#10)
	if _, _, code := cli(t, nil, "set-root", t.TempDir(), "--keep-old"); code != 2 {
		t.Fatalf("set-root accepted: exit %d", code)
	}
}
