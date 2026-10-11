package mcpserver

import (
	"strings"
	"testing"
)

func TestRenderStatusHead(t *testing.T) {
	lines := func(staying string) []StatusLine {
		return []StatusLine{
			{Text: "service: background service pid 1"},
			{Text: "root: /r", Label: "root", Static: true},
			{Text: "recording: idle"},
			{Text: "lifetime: exits 30s after …" + staying, Label: "lifetime", Static: true, Key: "lifetime: exits 30s after …"},
			{Text: "adb: ok (/adb, version 1)", Label: "adb", Static: true},
			{Text: "store: /r (data in v1/<device-id>)", Label: "store", Static: true},
		}
	}
	render := func(ls []StatusLine, m *StatusMemo, verbose bool) string {
		var b strings.Builder
		renderStatusHead(&b, ls, m, verbose)
		return b.String()
	}
	all := "service: background service pid 1\nroot: /r\nrecording: idle\nlifetime: exits 30s after …; staying: 1 client(s) connected\nadb: ok (/adb, version 1)\nstore: /r (data in v1/<device-id>)\n"
	short := "service: background service pid 1\nunchanged since shown earlier in this session: root, lifetime, adb, store (logcat_status(verbose=true) shows them)\nrecording: idle\n"

	// no memo (in-process --offline, tests): always everything
	for i := 0; i < 2; i++ {
		if got := render(lines("; staying: 1 client(s) connected"), nil, false); got != all {
			t.Fatalf("no memo, call %d:\n%s", i, got)
		}
	}
	m := &StatusMemo{}
	if got := render(lines("; staying: 1 client(s) connected"), m, false); got != all {
		t.Fatalf("first:\n%s", got)
	}
	// only the key counts: the lifetime's current state is not a change
	if got := render(lines("; staying: 3 client(s) connected"), m, false); got != short {
		t.Fatalf("second:\n%s", got)
	}
	if got := render(lines("; staying: 1 client(s) connected"), m, true); got != all {
		t.Fatalf("verbose:\n%s", got)
	}
	changed := lines("; staying: 1 client(s) connected")
	changed[4].Text = "adb: ok (/adb, version 2)"
	if got := render(changed, m, false); !strings.Contains(got, "adb: ok (/adb, version 2)\n") || strings.Contains(got, "unchanged") {
		t.Fatalf("changed:\n%s", got)
	}
	if got := render(changed, m, false); strings.Contains(got, "adb: ok") {
		t.Fatalf("after the change:\n%s", got)
	}
	// nothing static: nothing to say
	if got := render([]StatusLine{{Text: "adb: NOT FOUND — x"}}, m, false); got != "adb: NOT FOUND — x\n" {
		t.Fatalf("no static lines:\n%s", got)
	}
}
