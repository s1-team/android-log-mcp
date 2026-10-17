// Package layout owns the data root of android-log-mcp:
//
//	<root>/                       os.UserCacheDir()/android-log-mcp by default (set-root moves it)
//	  v1/<device-id>/             device data, format 1 (segments, meta.json, aux files)
//	  tmp/                        scratch files (expire after an hour)
//	  bin/<version>-<hash>/       service binaries (the running one + at most one other)
//	  log/                        service log (size-capped, rotated)
//	  export/                     export files (export.go: 7 days, 20 files, 256 MiB)
//	  run/                        only in the default root, which is also the service's
//	                              control directory (service lock, state, socket)
//
// Only the background service writes here (one per user, issue #3).
// Everything the tool manages under the root is bounded by the total size
// cap and the retention window (Sweep); anything else there (e.g.
// directories of versions before 0.4) is ignored: never read, never
// deleted.
package layout

import (
	"os"
	"path/filepath"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Root is a data root.
type Root struct {
	Dir string
}

// DefaultDir is os.UserCacheDir()/android-log-mcp (the temp dir if the
// cache dir is unknown).
func DefaultDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "android-log-mcp")
}

func (r Root) Tmp() string { return filepath.Join(r.Dir, "tmp") }

// DeviceDir is the format-1 data directory of device id under the root base.
func DeviceDir(base, id string) string {
	return filepath.Join(store.DataDir(base), store.SanitizeSerial(id))
}

// Ensure creates the managed sub-directories.
func (r Root) Ensure() error {
	for _, d := range []string{r.Tmp(), store.DataDir(r.Dir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return r.EnsureExport()
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
