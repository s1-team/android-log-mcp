//go:build windows

package procattr

import (
	"fmt"
	"os"
	"os/exec"
)

// RunConsoleProbe is the body of a test helper process: it appends
// "<role> hwnd=<h> visible=<bool>" to file out and, if spawnChild, runs the
// same test binary once more as a grandchild with a plain exec.Command
// (deliberately without Hide) so the caller can check what a forgotten
// child inherits. Returns the exit code.
func RunConsoleProbe(out string, spawnChild bool, testRun string) int {
	h, vis := ConsoleWindow()
	role := "grandchild"
	if spawnChild {
		role = "child"
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(f, "%s hwnd=%#x visible=%v\n", role, h, vis)
	f.Close()
	if spawnChild {
		c := exec.Command(os.Args[0], "-test.run="+testRun) // procattr:allow: deliberately not hidden, tests what a forgotten child inherits
		c.Env = append(os.Environ(), "ALM_CONSOLE_PROBE_CHILD=")
		if b, err := c.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "grandchild: %v\n%s", err, b)
			return 1
		}
	}
	return 0
}
