package main

import (
	"os"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// Never touch the real user's data root, owners directory or config file.
// With ALM_RUN_MAIN set, the test binary runs main() instead (CLI tests;
// the parent's TestMain already isolated the environment it passes on).
func TestMain(m *testing.M) {
	if os.Getenv("ALM_RUN_MAIN") != "" {
		os.Args = append([]string{"android-log-mcp"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(testhome.Run(m))
}
