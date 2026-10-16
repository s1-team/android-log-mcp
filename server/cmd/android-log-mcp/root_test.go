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
	// serial: uses the package-wide test home (testhome), shared with the other serial tests
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{[]string{"ANDROID_LOG_MCP_HOME=/tmp/x"}, []string{"call", "logcat_status"}},
		{nil, []string{"--home", "/tmp/x"}},
		{nil, []string{"--home=/tmp/x", "report"}},
	} {
		_, errOut, code := cli(t, c.env, c.args...)
		if code != 2 || !strings.Contains(errOut, "removed") || !strings.Contains(errOut, "set-root <dir>") {
			t.Fatalf("%v %v: code %d stderr %q", c.env, c.args, code, errOut)
		}
	}
}

func TestRootCommand(t *testing.T) {
	// serial: uses the package-wide test home (testhome), shared with the other serial tests
	out, _, code := cli(t, nil, "root")
	if code != 0 || strings.TrimSpace(out) != layout.DefaultDir() {
		t.Fatalf("root: %d %q", code, out)
	}
	out, _, code = cli(t, nil, "root", "--json")
	var r struct{ Root, Control, Config, Socket string }
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Root != layout.DefaultDir() || r.Control != layout.DefaultDir() || r.Socket == "" {
		t.Fatalf("root --json: %d %q", code, out)
	}
	// set-root needs exactly one of --keep-old / --delete-old (no prompts)
	d1, d2 := t.TempDir(), t.TempDir()
	for _, c := range []struct {
		args []string
		next string
	}{
		{[]string{"set-root", d1}, "next: android-log-mcp set-root " + shellQuote(d1) + " --keep-old"},
		{[]string{"set-root", d2, "--keep-old", "--delete-old"}, "next: android-log-mcp set-root " + shellQuote(d2) + " --keep-old"},
		{[]string{"set-root", "--keep-old"}, "next: android-log-mcp root"},
	} {
		if _, errOut, code := cli(t, nil, c.args...); code != 2 || !strings.HasPrefix(errOut, "error code=usage ") || !strings.Contains(errOut, c.next) || strings.Contains(errOut[strings.Index(errOut, "next:"):], "--delete-old") || strings.Contains(errOut[strings.Index(errOut, "next:"):], " | ") {
			t.Fatalf("%v: exit %d\n%s", c.args, code, errOut)
		}
	}
}
