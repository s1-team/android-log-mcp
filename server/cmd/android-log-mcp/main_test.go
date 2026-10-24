package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// Never touch the real user's data root, control directory or service.
// With ALM_RUN_MAIN set, the test binary runs main() instead (CLI and
// multi-process tests; the parent isolated the environment it passes on).
// Test-only knobs (never read by the real binary): ALM_TEST_VERSION,
// ALM_TEST_PROTO, ALM_TEST_MINPROTO, ALM_TEST_NO_SPAWN,
// ALM_TEST_SETROOT_FAIL.
func TestMain(m *testing.M) {
	if fakeadb.Is() { // lazy_test.go
		os.Exit(fakeadb.Main(os.Args[1:]))
	}
	if isFakeADB() {
		os.Exit(fakeADB(os.Args[1:]))
	}
	if os.Getenv("ALM_RUN_MAIN") != "" {
		if v := os.Getenv("ALM_TEST_VERSION"); v != "" {
			mcpserver.Version = v
		}
		if v, err := strconv.Atoi(os.Getenv("ALM_TEST_PROTO")); err == nil {
			service.Proto = v
		}
		if v, err := strconv.Atoi(os.Getenv("ALM_TEST_MINPROTO")); err == nil {
			service.MinProto = v
		}
		service.TestFailSetRoot = os.Getenv("ALM_TEST_SETROOT_FAIL")
		if os.Getenv("ALM_TEST_NO_SPAWN") != "" {
			service.NoSpawn = true
		}
		os.Args = append([]string{"android-log-mcp"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(testhome.Run(m))
}

// `call` reports a usage or JSON mistake as one coded line with an executable
// next step (#85), never a Go error or the usage block.
func TestCallErrorsAreCoded(t *testing.T) {
	// Usage/JSON refusals only; no service spawn — off windowsRaceHeavy (#267).
	t.Parallel()
	env := isoEnv(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"call"}, "error code=usage msg=missing_tool next: android-log-mcp call logcat_status"},
		{[]string{"call", "logcat_status", "{nope"}, "error code=bad_json col="},
		{[]string{"call", "logcat_status", "[1]"}, "error code=bad_json msg=not_a_json_object next: android-log-mcp call logcat_status"},
	} {
		out, errOut, code := cliEnv(t, env, c.args...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, c.want) || strings.Count(errOut, "\n") != 1 {
			t.Fatalf("%v: exit %d out %q err %q", c.args, code, out, errOut)
		}
	}
	// a service-less `record` state is one header and the next step stays executable
	out, _, code := cliEnv(t, env, "record", "--for", "0")
	if code != 2 || out != "" {
		t.Fatalf("record --for 0: %d %q", code, out)
	}
}
