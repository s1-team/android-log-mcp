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
	t.Parallel()
	linger := &lingerVal{}
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.5.0", linger.opt)
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
	static := []string{"\nroot: ", "\nlinger: ", "\nlifetime: ", "\nstore path="}
	full := func(what, out string) {
		t.Helper()
		for _, w := range static {
			if !strings.Contains(out, w) {
				t.Fatalf("%s: lacks %q:\n%s", what, w, out)
			}
		}
		if strings.Contains(out, "\nunchanged ") {
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
			"service: service process pid", // always: pid, version, up, connected
			"\nunchanged since=earlier_in_session lines=root,linger,lifetime,store next: verbose=true\n",
			"\nhint code=adb_not_found", // a problem is never hidden
			" devices=",
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
	// MCP/CLI process(es)" and the connected count, which is not a change of the block
	b := dial()
	full("other connection, first call", status(b, `{}`))
	trimmed("first connection again", status(a, `{}`))
	trimmed("other connection, second call", status(b, `{}`))

	// a changed line shows the whole block again, once
	linger.Set("30m")
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
