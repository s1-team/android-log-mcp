package redact

import (
	"strings"
	"testing"
)

func TestUserPaths(t *testing.T) {
	r := newT(t, Options{})
	for _, c := range []struct{ in, user, keep string }{
		{"open /home/alice/.cache/android-log-mcp/run/service.sock", "alice", "/.cache/android-log-mcp/run/service.sock"},
		{"root /Users/Bob.Smith/Library/Caches/x", "Bob.Smith", "/Library/Caches/x"},
		{`root C:\Users\carol\AppData\Local\android-log-mcp`, "carol", `\AppData\Local\android-log-mcp`},
		{`root c:\users\CAROL\AppData`, "CAROL", `\AppData`},
		{`root C:/Users/dave/AppData/Local`, "dave", "/AppData/Local"},
		{`root C:\Users\John Smith\AppData\Local`, "Smith", `\AppData\Local`},
		{`{"exe":"C:\\Users\\erin\\bin\\android-log-mcp.exe"}`, "erin", `\\bin\\android-log-mcp.exe"}`},
		{`root c:\\USERS\\erin\\bin`, "erin", `\\bin`},
		{`file:///home/frank/x.txt`, "frank", "/x.txt"},
		{`wsl /mnt/c/Users/gina/Documents`, "gina", "/Documents"},
		{`unc \\?\C:\Users\hank\AppData`, "hank", `\AppData`},
		{"user dir /home/ivan", "ivan", ""},
	} {
		out := positive(t, r, "username", c.in, c.user)
		if !strings.Contains(out, c.keep) {
			t.Errorf("%q → %q: lost %q", c.in, out, c.keep)
		}
	}
	// the same user keeps one number
	out := r.Text("/home/alice/a and C:\\Users\\alice\\b")
	if strings.Count(out, "[REDACTED:username#1]") != 2 {
		t.Errorf("same user, different numbers: %q", out)
	}
}

func TestUserPathsLeaveOtherPaths(t *testing.T) {
	r := newT(t, Options{})
	negative(t, r, "GET /users/123/profile")
	negative(t, r, "/data/user/0/com.foo/files")
	negative(t, r, `C:\Windows\System32\drivers`)
	negative(t, r, `C:\Program Files\Android\platform-tools\adb.exe`)
	negative(t, r, "/usr/home/")
	negative(t, r, "/system/bin/home/")
	negative(t, r, "see C:\\Users\\")
}

func TestHomeDirIsRedactedWhole(t *testing.T) {
	r := newT(t, Options{Home: "/srv/people/zed"})
	positive(t, r, "home_dir", "log in /srv/people/zed/.cache/x and /srv/people/zed", "zed")
	negative(t, r, "/srv/people/zedd/x")
	w := newT(t, Options{Home: `D:\Profiles\Zed`})
	positive(t, w, "home_dir", `at d:\profiles\ZED\AppData`, "ZED")
	positive(t, w, "home_dir", `{"p":"D:\\Profiles\\Zed\\x"}`, "Zed")
	positive(t, w, "home_dir", `at D:/profiles/zed/x`, "zed")
	if o := newT(t, Options{Home: "/"}).Text("/a/b"); o != "/a/b" {
		t.Errorf("root home must not redact: %q", o)
	}
}

func TestUserPathsDisabledAndMask(t *testing.T) {
	r := newT(t, Options{Disabled: true})
	if o := r.Text("/home/alice/x"); o != "/home/alice/x" {
		t.Errorf("--no-redact changed text: %q", o)
	}
	if o := r.Mask("device_id", "S1"); o != "S1" {
		t.Errorf("Mask with --no-redact: %q", o)
	}
	m := newT(t, Options{})
	if a, b := m.Mask("device_id", "S1"), m.Mask("device_id", "S1"); a != b || !strings.HasPrefix(a, "[REDACTED:device_id#") {
		t.Errorf("Mask: %q %q", a, b)
	}
}
