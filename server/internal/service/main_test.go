package service

import (
	"os"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

func TestMain(m *testing.M) {
	if os.Getenv("ALM_TEST_SLEEP") != "" { // a running copy for TestInstallFileReplacesRunningCopy
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	if code, ok := consoleProbe(); ok { // helper child of the Windows console test
		os.Exit(code)
	}
	// never use the real adb (or the user's devices) in these tests
	os.Setenv("ANDROID_LOG_MCP_ADB", os.DevNull+"-no-adb")
	os.Exit(testhome.Run(m))
}
