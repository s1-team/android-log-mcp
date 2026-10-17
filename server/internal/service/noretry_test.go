package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
)

// logcat_export is the one tool that is not retried after a lost
// connection: a retry could write a second file (#172). Every other tool
// is read-only and keeps the retry.
func TestNoRetryTools(t *testing.T) {
	t.Parallel()
	for _, d := range mcpserver.ToolDefs() {
		if noRetryTools[d.Name] != d.Writes && d.Name != "logcat_status" {
			t.Errorf("%s: writes=%v but no-retry=%v", d.Name, d.Writes, noRetryTools[d.Name])
		}
	}
	if !noRetryTools["logcat_export"] || noRetryTools["logcat_search"] || noRetryTools["logcat_status"] {
		t.Errorf("%v", noRetryTools)
	}
	lost := fmt.Errorf("call: %w", ipc.ErrClosed)
	text, ok := lostCall("logcat_export", lost)
	if !ok || !strings.HasPrefix(text, "error code=service_unreachable ") || !strings.Contains(text, "next: android-log-mcp service status") {
		t.Errorf("%v %q", ok, text)
	}
	if _, ok := lostCall("logcat_search", lost); ok {
		t.Error("a read-only tool is retried, not answered")
	}
	if _, ok := lostCall("logcat_export", errors.New("other")); ok {
		t.Error("only a lost connection is answered this way")
	}
	if _, ok := lostCall("logcat_export", nil); ok {
		t.Error("no error")
	}
}
