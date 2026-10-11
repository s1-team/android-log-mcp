package service

import (
	"context"
	"strings"
	"testing"
)

// logcat_status shows the lines that rarely change (root, linger, lifetime,
// service log, adb, store) on the first call of a connection, again when
// they change and always with verbose=true; each connection is tracked on its
// own; `service status` always shows them (#23).
func TestStatusStaticLinesPerConnection(t *testing.T) {
	t.Setenv(LingerEnv, "")
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.5.0")
	dial := func() *Conn {
		c, err := Dial(context.Background(), ctrl, "0.5.0", "mcp")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	status := func(c *Conn, args string) string {
		t.Helper()
		var r CallReply
		if err := c.Do(context.Background(), "call", CallArgs{Name: "logcat_status", Args: []byte(args)}, &r); err != nil || r.IsError {
			t.Fatalf("logcat_status %s: %v %s", args, err, r.Text)
		}
		return r.Text
	}
	static := []string{"\nroot: ", "\nlinger: ", "\nlifetime: ", "\nstore: "}
	full := func(what, out string) {
		t.Helper()
		for _, w := range static {
			if !strings.Contains(out, w) {
				t.Fatalf("%s: lacks %q:\n%s", what, w, out)
			}
		}
		if strings.Contains(out, "unchanged since") {
			t.Fatalf("%s: has the unchanged line:\n%s", what, out)
		}
	}
	trimmed := func(what, out string) {
		t.Helper()
		for _, w := range static {
			if strings.Contains(out, w) {
				t.Fatalf("%s: still has %q:\n%s", what, w, out)
			}
		}
		for _, w := range []string{
			"service: background service pid", // always: pid, version, up, clients
			"\nunchanged since shown earlier in this session: root, linger, lifetime, store (logcat_status(verbose=true) shows them)\n",
			"\nadb: NOT FOUND", // a problem is never hidden
			"\ndevices (",
		} {
			if !strings.Contains(out, w) {
				t.Fatalf("%s: lacks %q:\n%s", what, w, out)
			}
		}
	}

	a := dial()
	full("first call", status(a, `{}`))
	second := status(a, `{}`)
	trimmed("second call", second)
	full("verbose", status(a, `{"verbose":true}`))
	trimmed("after verbose", status(a, `{}`))

	// another connection is independent; its arrival changes "staying: N
	// client(s)" and the clients count, which is not a change of the block
	b := dial()
	full("other connection, first call", status(b, `{}`))
	trimmed("first connection again", status(a, `{}`))
	trimmed("other connection, second call", status(b, `{}`))

	// a changed line shows the whole block again, once
	t.Setenv(LingerEnv, "30m")
	out := status(a, `{}`)
	full("linger changed", out)
	if !strings.Contains(out, "linger: per device, after its last log tool call: 30m0s") {
		t.Fatalf("new linger missing:\n%s", out)
	}
	trimmed("after the change", status(a, `{}`))

	// service status (CLI process) is unchanged: always every line
	txt := s.Info().Text()
	for _, w := range []string{"\nroot: ", "\nlinger: ", "\nlifetime: "} {
		if !strings.Contains(txt, w) {
			t.Fatalf("service status lacks %q:\n%s", w, txt)
		}
	}
}
