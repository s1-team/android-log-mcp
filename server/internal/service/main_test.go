package service

import (
	"os"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

func TestMain(m *testing.M) {
	if code, ok := consoleProbe(); ok { // helper child of the Windows console test
		os.Exit(code)
	}
	// never use the real adb (or the user's devices) in these tests
	os.Setenv("ANDROID_LOG_MCP_ADB", os.DevNull+"-no-adb")
	os.Exit(testhome.Run(m))
}
