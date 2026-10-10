package device

import (
	"os"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// Never touch the real user's data root or owners directory.
func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }
