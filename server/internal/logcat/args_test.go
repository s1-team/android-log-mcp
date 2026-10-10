package logcat

import (
	"strings"
	"testing"
)

// API 29 logcat has no --uid and honours only one --pid: never use them.
func TestArgsNoUIDOrPIDFilter(t *testing.T) {
	for _, r := range []string{"", "2026-10-10 03:00:00.000"} {
		a := strings.Join(Args(r), " ")
		if strings.Contains(a, "--uid") || strings.Contains(a, "--pid") {
			t.Errorf("args use device-side filtering: %s", a)
		}
		for _, w := range []string{"-v printable", "-v uid", "-v epoch", "-b main,system,crash,events"} {
			if !strings.Contains(a, w) {
				t.Errorf("args lack %q: %s", w, a)
			}
		}
	}
}
