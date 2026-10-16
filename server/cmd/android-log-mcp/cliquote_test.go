package main

import (
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
)

func TestShellQuoteWindowsAndPosixPaths(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ goos, in, want string }{
		{"windows", `C:\x`, `C:\x`},
		{"windows", `C:\Program Files\alm`, `"C:\Program Files\alm"`},
		{"windows", `C:\a,b\c`, `"C:\a,b\c"`},
		{"windows", ``, `""`},
		{"linux", `/home/u/alm`, `/home/u/alm`},
		{"linux", `/home/u/my dir`, `'/home/u/my dir'`},
		{"linux", `/home/u/it's`, `'/home/u/it'\''s'`},
		{"darwin", `/Users/a,b/x`, `/Users/a,b/x`},
		{"windows", `\\srv\share`, `\\srv\share`},
		{"windows", `\\srv\my share\x`, `"\\srv\my share\x"`},
		{"linux", `\\srv\share`, `'\\srv\share'`},
		{"darwin", `\\srv\my share`, `'\\srv\my share'`},
	} {
		if got := shellQuoteFor(c.goos, c.in); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.goos, c.in, got, c.want)
		}
	}
	if got := removeCmd("windows", `C:\old root\v1`); got != `rmdir /s /q "C:\old root\v1"` {
		t.Errorf("windows remove: %s", got)
	}
	if got := removeCmd("linux", "/old root/v1"); got != "rm -rf '/old root/v1'" {
		t.Errorf("posix remove: %s", got)
	}
}

func TestSeveralNextIsOneCommandPerLine(t *testing.T) {
	t.Parallel()
	err := &severalPinnedError{ids: []string{"emulator-5554", "my phone"}}
	cmds := severalNext(err)
	if len(cmds) != 2 || cmds[0] != "android-log-mcp record --stop --device emulator-5554" || cmds[1] != "android-log-mcp record --stop --device "+shellQuote("my phone") {
		t.Fatalf("cmds %q", cmds)
	}
	out := mcpserver.CLIErrorAlts("several_devices", cmds, "msg", err.Error())
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "error code=several_devices ") || strings.Contains(lines[0], "next:") {
		t.Fatalf("output:\n%s", out)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "next: android-log-mcp record --stop --device ") {
			t.Errorf("not a standalone next line: %q", l)
		}
	}
	for _, l := range lines {
		i := strings.Index(l, "next: ")
		if i < 0 {
			continue
		}
		cmd := l[i:]
		if strings.Contains(cmd, " | ") || strings.Contains(cmd, ";") || strings.Contains(cmd, "&&") {
			t.Errorf("chained commands in %q", l)
		}
	}
	if got := severalNext(nil); len(got) != 1 || got[0] != "android-log-mcp record" {
		t.Errorf("fallback %q", got)
	}
	// a single alternative stays on the error line
	if one := mcpserver.CLIErrorAlts("x", []string{"android-log-mcp root"}); !strings.HasSuffix(one, " next: android-log-mcp root") || strings.Contains(one, "\n") {
		t.Errorf("single: %q", one)
	}
}
