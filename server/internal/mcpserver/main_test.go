package mcpserver

import (
	"os"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// Never touch the real user's data root or owners directory.
func TestMain(m *testing.M) {
	if fakeadb.Is() { // this test binary, copied as adb (lazy_test.go)
		os.Exit(fakeadb.Main(os.Args[1:]))
	}
	os.Exit(testhome.Run(m))
}
