package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tempDir is t.TempDir plus a check, run after every cleanup registered later
// (e.g. t.Cleanup(s.Close)), that the test left no file open inside it. On
// Windows an open handle makes the TempDir removal fail ("being used by
// another process"); /proc/self/fd lets Linux CI catch the same leak.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if runtime.GOOS != "linux" {
			return
		}
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return
		}
		for _, e := range ents {
			p, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
			if err == nil && strings.HasPrefix(p, dir+string(os.PathSeparator)) {
				t.Errorf("file still open at end of test: %s", p)
			}
		}
	})
	return dir
}
