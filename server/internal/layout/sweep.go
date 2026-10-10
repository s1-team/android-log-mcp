package layout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// SweepOptions configures Sweep.
type SweepOptions struct {
	// Base is the data root (the default dir; a test's temp root).
	Base string
	// KeepBin is the bin/ directory of the running service binary: never
	// removed. Besides it, the newest other bin/ entry is kept.
	KeepBin string
	// MaxTotal caps all managed files under Base (0 = no cap).
	MaxTotal int64
	// MaxAge is the retention window; 0 = no age expiry.
	MaxAge time.Duration
	// TmpAge expires files in tmp/ (default 1 h).
	TmpAge time.Duration
	// Owned are the device directories this process records (pruned by
	// their own store, never deleted from under it).
	Owned map[string]*store.Store
	Now   time.Time
}

// SweepResult summarizes what Sweep removed.
type SweepResult struct {
	Freed        int64    // bytes removed
	Files        int      // files removed
	RemovedDirs  []string // device directories removed entirely
	StaleRun     int      // run/ entries removed (dead pids, stale sockets)
	StaleOwners  int      // leftover owners/*.lock of 0.4 development builds removed
	OldBins      int      // bin/ versions removed
	TotalBefore  int64
	TotalAfter   int64
	CapTriggered bool
}

var versionDir = regexp.MustCompile(`^v[0-9]+$`)

// known file names of a device directory (only these are ever deleted from
// one, so a root set to a directory with other content cannot lose foreign files)
func deviceFile(name string) bool {
	switch {
	case strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".jsonl"),
		strings.HasSuffix(name, ".jsonl"), strings.HasSuffix(name, ".tmp"),
		strings.HasPrefix(name, "dropbox_status_"),
		name == "meta.json", name == "packages.json", name == "writer.lock":
		return true
	}
	return false
}

type dataDir struct {
	path    string
	current bool // v<DataFormat>/<id>: keep its newest segment under the cap
	id      string
}

func isDeviceDir(p string) bool {
	if exists(filepath.Join(p, "meta.json")) {
		return true
	}
	return len(store.ListSegments(p)) > 0
}

// deviceDirs lists every device directory under base: v<N>/* of any format
// version. Other directories (e.g. pre-0.4 <base>/<serial>) are ignored.
func deviceDirs(base string) []dataDir {
	var out []dataDir
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		if versionDir.MatchString(e.Name()) {
			cur := e.Name() == filepath.Base(store.DataDir(base))
			sub, _ := os.ReadDir(p)
			for _, s := range sub {
				if s.IsDir() && isDeviceDir(filepath.Join(p, s.Name())) {
					out = append(out, dataDir{path: filepath.Join(p, s.Name()), current: cur, id: s.Name()})
				}
			}
		}
	}
	return out
}

// TotalBytes is the size of all files Sweep manages under base (device
// directories of every format, tmp/, run/, bin/, log/, owners/ if there).
func TotalBytes(base string) int64 {
	var total int64
	add := func(dir string, recursive bool) {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != dir && !recursive {
					return filepath.SkipDir
				}
				return nil
			}
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
			return nil
		})
	}
	for _, dd := range deviceDirs(base) {
		add(dd.path, false)
	}
	for _, d := range []string{"tmp", "run", "owners", "bin", "log"} {
		add(filepath.Join(base, d), true)
	}
	return total
}

// Sweep applies retention to everything the tool manages under the root:
// stale run/ entries, tmp/ files, old bin/ versions, the MaxAge window for
// device directories this process does not record (every format version,
// removing a directory once all of it has expired), and finally the total
// size cap (oldest segments first across all devices; bin/ and log/ count
// but are bounded by their own rules). Directories written by another live
// process (writer.lock held) are left to it. Unknown directories (e.g.
// from versions before 0.4) are ignored.
func Sweep(o SweepOptions) SweepResult {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.TmpAge <= 0 {
		o.TmpAge = time.Hour
	}
	var res SweepResult
	res.StaleRun = SweepRun(filepath.Join(o.Base, "run"), o.MaxAge, o.Now)
	res.StaleOwners = sweepOldOwners(filepath.Join(o.Base, "owners"))
	res.OldBins = SweepBin(filepath.Join(o.Base, "bin"), o.KeepBin)
	tmpBase := filepath.Join(o.Base, "tmp")
	res.addAll(removeOlder(tmpBase, o.Now.Add(-o.TmpAge)))
	busy := func(dd dataDir) bool {
		if o.Owned[dd.path] != nil {
			return true
		}
		return store.WriterLockHeld(dd.path)
	}
	dirs := deviceDirs(o.Base)
	// age expiry
	if o.MaxAge > 0 {
		cut := o.Now.Add(-o.MaxAge)
		for _, dd := range dirs {
			if busy(dd) {
				continue
			}
			res.expireDir(dd, cut)
		}
	}
	// total cap
	res.TotalBefore = TotalBytes(o.Base)
	total := res.TotalBefore
	if o.MaxTotal > 0 && total > o.MaxTotal {
		res.CapTriggered = true
		type cand struct {
			dd   dataDir
			path string
			size int64
			mod  time.Time
		}
		var cands []cand
		for _, dd := range deviceDirs(o.Base) {
			segs := store.ListSegments(dd.path)
			if dd.current || o.Owned[dd.path] != nil {
				if len(segs) > 0 {
					segs = segs[:len(segs)-1] // keep each recorded device's newest segment
				}
			}
			for _, sg := range segs {
				if fi, err := os.Stat(sg.Path); err == nil {
					cands = append(cands, cand{dd, sg.Path, sg.Size, fi.ModTime()})
				}
			}
		}
		for _, d := range []string{tmpBase} {
			filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if e.IsDir() {
					if p != d && isDeviceDir(p) {
						return filepath.SkipDir // its segments are candidates above
					}
					return nil
				}
				if fi, err := e.Info(); err == nil {
					cands = append(cands, cand{path: p, size: fi.Size(), mod: fi.ModTime()})
				}
				return nil
			})
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].mod.Before(cands[j].mod) })
		skip := map[string]bool{}
		for _, c := range cands {
			if total <= o.MaxTotal {
				break
			}
			if c.dd.path == "" { // tmp file
				if os.Remove(c.path) == nil {
					total -= c.size
					res.Freed += c.size
					res.Files++
				}
				continue
			}
			if skip[c.dd.path] {
				continue
			}
			if st := o.Owned[c.dd.path]; st != nil {
				if n, ok := st.RemoveOldestSegment(c.path); ok {
					total -= n
					res.Freed += n
					res.Files++
				} else {
					skip[c.dd.path] = true
				}
				continue
			}
			if busy(c.dd) {
				skip[c.dd.path] = true
				continue
			}
			f, ok := store.TryLock(filepath.Join(c.dd.path, "writer.lock"))
			if !ok {
				skip[c.dd.path] = true
				continue
			}
			err := os.Remove(c.path)
			store.ReleaseLock(f)
			if err != nil {
				skip[c.dd.path] = true
				continue
			}
			total -= c.size
			res.Freed += c.size
			res.Files++
		}
		// a non-current device dir whose segments are all gone goes entirely
		for _, dd := range deviceDirs(o.Base) {
			if dd.current || busy(dd) || len(store.ListSegments(dd.path)) > 0 {
				continue
			}
			res.removeDir(dd.path)
		}
	}
	ents, _ := os.ReadDir(o.Base)
	for _, e := range ents {
		if e.IsDir() && versionDir.MatchString(e.Name()) && e.Name() != filepath.Base(store.DataDir(o.Base)) {
			pruneEmpty(filepath.Join(o.Base, e.Name()), true)
		}
	}
	res.TotalAfter = TotalBytes(o.Base)
	return res
}

func (r *SweepResult) addAll(n int64, files int) {
	r.Freed += n
	r.Files += files
}

// expireDir deletes the segments of an unrecorded device directory whose
// last write is older than cut (including the newest one), and removes the
// directory once nothing in it is newer than cut.
func (r *SweepResult) expireDir(dd dataDir, cut time.Time) {
	f, ok := store.TryLock(filepath.Join(dd.path, "writer.lock"))
	if !ok {
		return
	}
	newest := time.Time{}
	ents, _ := os.ReadDir(dd.path)
	for _, e := range ents {
		if e.IsDir() || e.Name() == "writer.lock" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), "seg-") && strings.HasSuffix(e.Name(), ".jsonl") && fi.ModTime().Before(cut) {
			if os.Remove(filepath.Join(dd.path, e.Name())) == nil {
				r.Freed += fi.Size()
				r.Files++
			}
			continue
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	store.ReleaseLock(f)
	if newest.Before(cut) && len(store.ListSegments(dd.path)) == 0 {
		r.removeDir(dd.path)
	}
}

// removeDir deletes a device directory's known files and then the directory.
func (r *SweepResult) removeDir(dir string) {
	f, ok := store.TryLock(filepath.Join(dir, "writer.lock"))
	if !ok {
		return
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() || e.Name() == "writer.lock" || !deviceFile(e.Name()) {
			continue
		}
		fi, _ := e.Info()
		if os.Remove(filepath.Join(dir, e.Name())) == nil && fi != nil {
			r.Freed += fi.Size()
			r.Files++
		}
	}
	os.Remove(filepath.Join(dir, "writer.lock")) // unix: while holding it
	store.ReleaseLock(f)
	os.Remove(filepath.Join(dir, "writer.lock"))
	if os.Remove(dir) == nil {
		r.RemovedDirs = append(r.RemovedDirs, dir)
	}
}

// removeOlder deletes regular files under dir (recursively) last modified
// before cut and then empty sub-directories. Device directories inside are
// skipped (they expire through expireDir).
func removeOlder(dir string, cut time.Time) (int64, int) {
	var freed int64
	n := 0
	filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			if p != dir && isDeviceDir(p) {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := e.Info()
		if err == nil && fi.ModTime().Before(cut) && os.Remove(p) == nil {
			freed += fi.Size()
			n++
		}
		return nil
	})
	pruneEmpty(dir, false)
	return freed, n
}

// pruneEmpty removes empty directories below dir (and dir itself if self).
func pruneEmpty(dir string, self bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			pruneEmpty(filepath.Join(dir, e.Name()), true)
		}
	}
	if self {
		if ents, _ := os.ReadDir(dir); len(ents) == 1 && ents[0].Name() == "writer.lock" {
			lp := filepath.Join(dir, "writer.lock")
			if f, ok := store.TryLock(lp); ok {
				os.Remove(lp)
				store.ReleaseLock(f)
			}
		}
		os.Remove(dir) // fails unless empty
	}
}

// sweepOldOwners removes the owners/ directory of 0.4 development builds
// (per-device owner locks, replaced by the single service): lock files
// nobody holds, then the directory if it is empty.
func sweepOldOwners(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, ok := store.TryLock(p)
		if !ok {
			continue // a development build still runs
		}
		os.Remove(p)
		store.ReleaseLock(f)
		n++
	}
	os.Remove(dir) // only if empty
	return n
}

// binFile: the only names ever deleted from a bin/<version>/ directory.
func binFile(name string) bool {
	return name == "android-log-mcp" || name == "android-log-mcp.exe" || strings.HasPrefix(name, ".install-")
}

// SweepBin keeps the running service's bin/ directory (keep) plus the most
// recently installed other one and removes the rest (only known file
// names; a binary still in use elsewhere, which Windows refuses to delete,
// is left for a later sweep). Returns how many directories it removed.
func SweepBin(binDir, keep string) int {
	ents, err := os.ReadDir(binDir)
	if err != nil {
		return 0
	}
	type ver struct {
		path string
		mod  time.Time
	}
	var vs []ver
	for _, e := range ents {
		p := filepath.Join(binDir, e.Name())
		if !e.IsDir() || (keep != "" && filepath.Clean(p) == filepath.Clean(keep)) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		mod := fi.ModTime() // the binary's mtime = when it was last installed / started
		for _, n := range []string{"android-log-mcp", "android-log-mcp.exe"} {
			if bi, err := os.Stat(filepath.Join(p, n)); err == nil {
				mod = bi.ModTime()
			}
		}
		vs = append(vs, ver{p, mod})
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].mod.After(vs[j].mod) })
	n := 0
	for i, v := range vs {
		if i == 0 {
			continue // the newest other version (e.g. just installed by a client)
		}
		files, _ := os.ReadDir(v.path)
		for _, f := range files {
			if binFile(f.Name()) {
				os.Remove(filepath.Join(v.path, f.Name()))
			}
		}
		if os.Remove(v.path) == nil {
			n++
		}
	}
	return n
}

// RunState is a run/<name>.json file describing a live process endpoint.
type RunState struct {
	PID    int    `json:"pid"`
	Socket string `json:"socket,omitempty"`
}

// SweepRun removes stale runtime entries from run/: state files whose
// process is gone or whose socket no longer accepts connections, sockets
// nothing listens on, and any other file older than maxAge.
func SweepRun(dir string, maxAge time.Duration, now time.Time) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	live := map[string]bool{}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(p)
		var st RunState
		if err != nil || json.Unmarshal(b, &st) != nil {
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > time.Minute {
				os.Remove(p) // unreadable (and not being written right now)
				n++
			}
			continue
		}
		ok := st.PID > 0 && pidAlive(st.PID)
		if ok && st.Socket != "" {
			ok = socketLive(st.Socket)
		}
		if !ok {
			os.Remove(p)
			n++
			if st.Socket != "" && filepath.Dir(st.Socket) == dir {
				if os.Remove(st.Socket) == nil {
					n++
				}
			}
			continue
		}
		if st.Socket != "" {
			live[filepath.Clean(st.Socket)] = true
		}
	}
	ents, _ = os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil || strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".lock") {
			continue // lock files: deleting a held one would let a second holder in
		}
		switch {
		case fi.Mode()&os.ModeSocket != 0 || strings.HasSuffix(e.Name(), ".sock"):
			if !live[filepath.Clean(p)] && !socketLive(p) {
				if os.Remove(p) == nil {
					n++
				}
			}
		case maxAge > 0 && now.Sub(fi.ModTime()) > maxAge:
			if os.RemoveAll(p) == nil {
				n++
			}
		}
	}
	return n
}
