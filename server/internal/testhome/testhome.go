// Package testhome isolates tests from the real user's data root, the
// fixed per-user owners directory (os.UserCacheDir()/android-log-mcp/owners)
// and the per-user config file (os.UserConfigDir()/android-log-mcp/config.json,
// which sets the data root of every process).
//
// Use it from TestMain in any package whose tests create a device.Manager
// or call layout.DefaultDir / DefaultOwnersDir / ConfigPath / ConfiguredRoot / SetRoot:
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

// Run points XDG_CACHE_HOME, XDG_CONFIG_HOME, HOME, LocalAppData, AppData
// (the inputs of os.UserCacheDir / os.UserConfigDir on Linux, macOS and
// Windows) at a fresh temp directory, clears the removed
// ANDROID_LOG_MCP_HOME, runs the tests and removes the directory. It refuses
// to run if the cache or config dir still resolves outside that directory.
func Run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "alm-testhome-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	for k, v := range map[string]string{
		"XDG_CACHE_HOME":  filepath.Join(dir, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		"AppData":         filepath.Join(dir, "AppData", "Roaming"),
		"HOME":            dir,
		"USERPROFILE":     dir,
		"LocalAppData":    filepath.Join(dir, "AppData", "Local"),
	} {
		os.Setenv(k, v)
	}
	os.Unsetenv("ANDROID_LOG_MCP_HOME")
	c, err := os.UserCacheDir()
	if err != nil || !strings.HasPrefix(c, dir) {
		fmt.Fprintf(os.Stderr, "testhome: user cache dir %q not isolated under %s\n", c, dir)
		return 1
	}
	if c, err := os.UserConfigDir(); err != nil || !strings.HasPrefix(c, dir) {
		fmt.Fprintf(os.Stderr, "testhome: user config dir %q not isolated under %s\n", c, dir)
		return 1
	}
	return m.Run()
}
