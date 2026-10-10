package main

import (
	"os"
	"strconv"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// Never touch the real user's data root, control directory or service.
// With ALM_RUN_MAIN set, the test binary runs main() instead (CLI and
// multi-process tests; the parent isolated the environment it passes on).
// Test-only knobs (never read by the real binary): ALM_TEST_VERSION,
// ALM_TEST_PROTO, ALM_TEST_MINPROTO, ALM_TEST_NO_SPAWN.
func TestMain(m *testing.M) {
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
		if os.Getenv("ALM_TEST_NO_SPAWN") != "" {
			service.NoSpawn = true
		}
		os.Args = append([]string{"android-log-mcp"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(testhome.Run(m))
}
