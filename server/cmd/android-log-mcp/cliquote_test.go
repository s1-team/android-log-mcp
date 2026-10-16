package main

import "testing"

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

func TestSeveralNextIsExecutable(t *testing.T) {
	t.Parallel()
	got := severalNext(&severalPinnedError{ids: []string{"emulator-5554", "my phone"}})
	want := "android-log-mcp record --stop --device " + shellQuote("emulator-5554") + " | android-log-mcp record --stop --device " + shellQuote("my phone")
	if got != want || severalNext(nil) != "android-log-mcp record" {
		t.Errorf("got %q", got)
	}
}
