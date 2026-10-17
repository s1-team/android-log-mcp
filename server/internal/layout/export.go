package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Export files (issue #172): <root>/export/export-<device-id>-<host time>.txt
// are written by the service process for human review and kept for a
// limited time. The limits are constants, not settings.
const (
	ExportDirName  = "export"
	ExportMaxAge   = 7 * 24 * time.Hour // older export files are removed
	ExportMaxFiles = 20                 // newest files kept
	ExportMaxBytes = 256 << 20          // total size of export files
	exportTmpAge   = time.Hour          // a leftover .export-*.tmp older than this is removed
	exportTmpGlob  = ".export-*.tmp"
)

// exportFile matches the only names the sweep ever deletes from export/ (a
// directory with foreign files, for example after set-root to a shared
// directory, loses none of them).
var exportFile = regexp.MustCompile(`^export-.+\.txt$`)

// Export is <root>/export.
func (r Root) Export() string { return filepath.Join(r.Dir, ExportDirName) }

// EnsureExport creates export/ for the current user only: mode 0700, a
// protected ACL on Windows (the same helper as the socket directory).
func (r Root) EnsureExport() error { return ipc.PrepareDir(r.Export()) }

// ExportName is the file name export-<label>-<YYYYMMDD-HHMMSS>[-n].txt.
// label is made safe as a file name (no separators, colons or spaces); t is
// host time; n >= 2 disambiguates a name that is taken.
func ExportName(label string, t time.Time, n int) string {
	l := store.SanitizeSerial(label)
	if len(l) > 64 {
		l = l[:64]
	}
	l = strings.TrimRight(l, ".")
	suffix := ""
	if n >= 2 {
		suffix = fmt.Sprintf("-%d", n)
	}
	return fmt.Sprintf("export-%s-%s%s.txt", l, t.Format("20060102-150405"), suffix)
}

// ErrExists is returned by AtomicFile.Commit when the target exists and
// overwrite was not asked for.
var ErrExists = errors.New("file exists")

// AtomicFile is a file written under a temporary name in the directory of its
// final path and renamed when complete, so a reader never sees a partial
// file. Close the temp file before the rename (Windows cannot rename an open
// file). Abort (or a failed Commit) removes the temp file.
type AtomicFile struct {
	*os.File
	dir  string
	done bool
}

// NewAtomicFile creates a 0600 temp file in dir (which must exist).
func NewAtomicFile(dir string) (*AtomicFile, error) {
	f, err := os.CreateTemp(dir, exportTmpGlob)
	if err != nil {
		return nil, err
	}
	return &AtomicFile{File: f, dir: dir}, nil
}

// Abort closes and removes the temp file; safe after Commit (then a no-op).
func (a *AtomicFile) Abort() {
	if a.done {
		return
	}
	a.done = true
	a.File.Close()
	os.Remove(a.File.Name())
}

// Commit syncs and closes the temp file and renames it to final (in the same
// directory). Without overwrite an existing final is ErrExists (the check
// and the rename are not atomic together; one service process writes here).
func (a *AtomicFile) Commit(final string, overwrite bool) error {
	if a.done {
		return errors.New("already committed or aborted")
	}
	if filepath.Dir(final) != a.dir {
		a.Abort()
		return fmt.Errorf("%s is not in %s", final, a.dir)
	}
	if !overwrite {
		if _, err := os.Lstat(final); err == nil {
			a.Abort()
			return ErrExists
		}
	}
	if err := a.File.Sync(); err != nil {
		a.Abort()
		return err
	}
	tmp := a.File.Name()
	if err := a.File.Close(); err != nil {
		a.done = true
		os.Remove(tmp)
		return err
	}
	a.done = true
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// FreeName returns the first name(n), n = 1, 2, ..., that does not exist in dir.
func FreeName(dir string, name func(n int) string) (string, error) {
	for n := 1; n < 1000; n++ {
		p := filepath.Join(dir, name(n))
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", errors.New("no free file name")
}

// SweepExport applies the retention of export/ and returns the bytes and
// files removed: leftover temp files older than an hour, files older than
// maxAge, then the oldest files while there are more than maxFiles or more
// than maxBytes. Zero limits mean the defaults above; a negative one turns
// that limit off. A file that cannot be removed (open in an editor on
// Windows) is skipped and retried on the next sweep; it still counts
// toward the limits so other files go instead.
func SweepExport(dir string, now time.Time, maxAge time.Duration, maxFiles int, maxBytes int64) (int64, int) {
	if maxAge == 0 {
		maxAge = ExportMaxAge
	}
	if maxFiles == 0 {
		maxFiles = ExportMaxFiles
	}
	if maxBytes == 0 {
		maxBytes = ExportMaxBytes
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var freed int64
	n := 0
	rm := func(p string, size int64) bool {
		if os.Remove(p) != nil {
			return false
		}
		freed += size
		n++
		return true
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var fs []file
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if ok, _ := filepath.Match(exportTmpGlob, e.Name()); ok {
			if now.Sub(fi.ModTime()) > exportTmpAge {
				rm(p, fi.Size())
			}
			continue
		}
		if exportFile.MatchString(e.Name()) {
			fs = append(fs, file{p, fi.Size(), fi.ModTime()})
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) }) // oldest first
	var total int64
	for _, f := range fs {
		total += f.size
	}
	count := len(fs)
	for _, f := range fs {
		old := maxAge > 0 && now.Sub(f.mod) > maxAge
		if old || (maxFiles > 0 && count > maxFiles) || (maxBytes > 0 && total > maxBytes) {
			if rm(f.path, f.size) {
				count--
				total -= f.size
			}
		}
	}
	return freed, n
}
