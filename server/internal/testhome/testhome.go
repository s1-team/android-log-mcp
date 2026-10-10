// Package testhome isolates tests from the real user's data root and,
// above all, from the fixed per-user owners directory
// (os.UserCacheDir()/android-log-mcp/owners), which every root shares.
//
// Use it from TestMain in any package whose tests create a device.Manager
// or call layout.DefaultDir / DefaultOwnersDir / device.DefaultBase:
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }
package testhome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run points XDG_CACHE_HOME, HOME, LocalAppData (the inputs of
// os.UserCacheDir on Linux, macOS and Windows) at a fresh temp directory,
// clears ANDROID_LOG_MCP_HOME, runs the tests and removes the directory.
// It refuses to run if the cache dir still resolves outside that directory.
func Run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "alm-testhome-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	for k, v := range map[string]string{
		"XDG_CACHE_HOME": filepath.Join(dir, "cache"),
		"HOME":           dir,
		"USERPROFILE":    dir,
		"LocalAppData":   filepath.Join(dir, "AppData", "Local"),
	} {
		os.Setenv(k, v)
	}
	os.Unsetenv("ANDROID_LOG_MCP_HOME")
	c, err := os.UserCacheDir()
	if err != nil || !strings.HasPrefix(c, dir) {
		fmt.Fprintf(os.Stderr, "testhome: user cache dir %q not isolated under %s\n", c, dir)
		return 1
	}
	return m.Run()
}
