package device

import (
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
)

// Device lines are dated by the epoch column, never by a year taken from the
// host clock: logcat.Assembler.Year (the host year at the call sites) is only
// a fallback for non-epoch lines, so every logcat read must ask for
// `-v epoch` (issue #107, docs/glossary.md "Clocks and times").
func TestEveryLogcatReadUsesEpoch(t *testing.T) {
	for name, cmd := range map[string]string{
		"stream":    strings.Join(logcat.Args(""), " "),
		"resume":    strings.Join(logcat.Args("1.000000"), " "),
		"reconcile": reconcileCmd(10),
		"t_end":     TEndCmd,
	} {
		if !strings.Contains(cmd, "-v epoch") {
			t.Errorf("%s: %q lacks -v epoch", name, cmd)
		}
	}
}
