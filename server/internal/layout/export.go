package layout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// AtomicFile is a file written under a temporary name and renamed when
// complete, so a reader never sees a partial file. The temp file is created
// in the directory NewAtomicFile is given (export/ for every export, issue
// #206: a crash must not leave a .tmp in a directory of the user's). Close
// the temp file before the rename (Windows cannot rename an open file).
// Abort (or a failed Commit) removes the temp file.
type AtomicFile struct {
	*os.File
	dir  string
	done bool
}

// renameFile is os.Rename; tests replace it to simulate a cross-volume move.
var renameFile = os.Rename

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

// Commit syncs and closes the temp file and renames it to final. When final
// is in another directory and the rename fails (another volume), the content
// is copied to a temp file in final's directory and renamed there; that
// directory must exist. Without overwrite an existing final is ErrExists (the
// check and the rename are not atomic together; one service process writes
// here). The temp file is removed in every case.
func (a *AtomicFile) Commit(final string, overwrite bool) error {
	if a.done {
		return errors.New("already committed or aborted")
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
	defer os.Remove(tmp) // no-op after a successful rename
	err := renameFile(tmp, final)
	if err == nil || filepath.Dir(final) == a.dir {
		return err
	}
	return copyAcross(tmp, final)
}

// copyAcross copies src to a temp file next to final and renames that.
func copyAcross(src, final string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(final), exportTmpGlob)
	if err != nil {
		return err
	}
	tmp := out.Name()
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = renameFile(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
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

// ProtectedOut reports whether an export written to path (`export --out`)
// would land in android-log-mcp's own structure under one of the bases (the
// data root, the control directory): a version directory, run/, tmp/, bin/,
// log/, export/ or config.json. Symlinks are resolved, and the comparison is
// by file identity (os.SameFile) wherever the entries exist, so a different
// letter case on a case-insensitive file system (Windows, macOS) or an 8.3
// short name cannot get around it; where the entry does not exist yet the
// names are compared case-insensitively on those systems. The returned name
// is the protected entry.
func ProtectedOut(path string, bases ...string) (string, bool) {
	p, err := ResolvePath(path)
	if err != nil {
		return "", false
	}
	for _, b := range bases {
		if b == "" {
			continue
		}
		rb, err := ResolvePath(b)
		if err != nil {
			continue
		}
		bi, err := os.Stat(rb)
		if err != nil {
			continue
		}
		// walk up from p to the directory that is the base (by identity)
		var comps []string
		cur := p
		for {
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			comps = append([]string{filepath.Base(cur)}, comps...)
			cur = parent
			if fi, err := os.Stat(cur); err == nil && os.SameFile(fi, bi) {
				if name, ok := protectedEntry(rb, comps[0]); ok {
					return name, true
				}
				break
			}
		}
	}
	return "", false
}

var foldNames = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// protectedEntry reports whether first, a name directly under base, is (or on
// a case-insensitive system could be) one of our own entries.
func protectedEntry(base, first string) (string, bool) {
	own := func(n string, dir bool) bool {
		return n == "run" || n == "config.json" || OwnEntry(n, dir, false)
	}
	if own(first, true) {
		return filepath.Join(base, first), true
	}
	if foldNames {
		for _, n := range []string{"run", "config.json", "tmp", "bin", "log", ExportDirName} {
			if strings.EqualFold(n, first) {
				return filepath.Join(base, first), true
			}
		}
		if m := versionDir.MatchString(strings.ToLower(first)); m {
			return filepath.Join(base, first), true
		}
	}
	fi, err := os.Stat(filepath.Join(base, first))
	if err != nil {
		return "", false
	}
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if !own(e.Name(), e.IsDir()) {
			continue
		}
		if ei, err := os.Stat(filepath.Join(base, e.Name())); err == nil && os.SameFile(ei, fi) {
			return filepath.Join(base, e.Name()), true
		}
	}
	return "", false
}

// ExportInfo describes an export file (NewestExport).
type ExportInfo struct {
	Path     string
	Size     int64
	Mod      time.Time
	Complete bool // has the `# end` footer and was not truncated
}

// NewestExport returns the most recently written export file of dir.
func NewestExport(dir string) (ExportInfo, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ExportInfo{}, false
	}
	var best ExportInfo
	found := false
	for _, e := range ents {
		if e.IsDir() || !exportFile.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if !found || fi.ModTime().After(best.Mod) {
			best, found = ExportInfo{Path: filepath.Join(dir, e.Name()), Size: fi.Size(), Mod: fi.ModTime()}, true
		}
	}
	if !found {
		return ExportInfo{}, false
	}
	if f, err := os.Open(best.Path); err == nil {
		defer f.Close()
		n := min(best.Size, 2048)
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, best.Size-n); err == nil || err == io.EOF {
			tail := string(buf)
			if i := strings.LastIndex(tail, "\n# end "); i >= 0 {
				best.Complete = strings.Contains(tail[i:], " truncated=0 ")
			}
		}
	}
	return best, true
}
