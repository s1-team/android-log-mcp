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
	// Base holds the device data: the root itself, or ANDROID_LOG_MCP_HOME.
	Base string
	// Root is the per-user root (owners/, run/, tmp/); nil = Base.
	Root *Root
	// MaxTotal caps all managed files under Base (0 = no cap).
	MaxTotal int64
	// MaxAge is the retention window; 0 = no age expiry.
	MaxAge time.Duration
	// TmpAge expires files in tmp/ (default 1 h).
	TmpAge time.Duration
	// Owned are the device directories this process records (pruned by
	// their own store, never deleted from under it).
	Owned map[string]*store.Store
	// KeepLegacy (ANDROID_LOG_MCP_LEGACY=keep) never deletes pre-v1 flat
	// directories or legacy/ (they still count towards the total).
	KeepLegacy bool
	Now        time.Time
}

// SweepResult summarizes what Sweep removed.
type SweepResult struct {
	Freed        int64    // bytes removed
	Files        int      // files removed
	RemovedDirs  []string // device / legacy directories removed entirely
	StaleRun     int      // run/ entries removed (dead pids, stale sockets)
	StaleOwners  int      // abandoned owner lock files removed
	TotalBefore  int64
	TotalAfter   int64
	CapTriggered bool
}

var versionDir = regexp.MustCompile(`^v[0-9]+$`)

// known file names of a device directory (only these are ever deleted from
// one, so a misconfigured ANDROID_LOG_MCP_HOME cannot lose foreign files)
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
	legacy  bool // flat pre-v1 directory or inside legacy/
}

func isDeviceDir(p string) bool {
	if exists(filepath.Join(p, "meta.json")) {
		return true
	}
	return len(store.ListSegments(p)) > 0
}

// deviceDirs lists every device directory under base: v<N>/* (any format
// version), flat ones directly under base, and those inside legacy/.
func deviceDirs(base string) []dataDir {
	var out []dataDir
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		switch {
		case versionDir.MatchString(e.Name()):
			cur := e.Name() == filepath.Base(store.DataDir(base))
			sub, _ := os.ReadDir(p)
			for _, s := range sub {
				if s.IsDir() && isDeviceDir(filepath.Join(p, s.Name())) {
					out = append(out, dataDir{path: filepath.Join(p, s.Name()), current: cur, id: s.Name()})
				}
			}
		case e.Name() == "legacy":
			filepath.WalkDir(p, func(q string, d os.DirEntry, err error) error {
				if err == nil && d.IsDir() && q != p && isDeviceDir(q) {
					out = append(out, dataDir{path: q, legacy: true})
				}
				return nil
			})
		case e.Name() == "owners" || e.Name() == "run" || e.Name() == "tmp":
		default:
			if isDeviceDir(p) {
				out = append(out, dataDir{path: p, legacy: true})
			}
		}
	}
	return out
}

// TotalBytes is the size of all files Sweep manages under base (device
// directories of every format, legacy/, tmp/, run/, owners/).
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
	legacy := filepath.Join(base, "legacy") + string(filepath.Separator)
	for _, dd := range deviceDirs(base) {
		if !strings.HasPrefix(dd.path, legacy) { // legacy/ is walked whole below
			add(dd.path, false)
		}
	}
	for _, d := range []string{"legacy", "tmp", "run", "owners"} {
		add(filepath.Join(base, d), true)
	}
	return total
}

// Sweep applies retention to everything under the root: stale run/ entries
// and owner locks, tmp/ files, the MaxAge window for device directories this
// process does not record (all formats and legacy data, removing a directory
// once all of it has expired), and finally the total size cap (oldest
// segments first across all devices). Directories recorded by a live
// process (its writer.lock or owner lock is held) are left to that process.
func Sweep(o SweepOptions) SweepResult {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.TmpAge <= 0 {
		o.TmpAge = time.Hour
	}
	root := o.Root
	if root == nil {
		root = &Root{Dir: o.Base}
	}
	var res SweepResult
	res.StaleRun = SweepRun(root.Run(), o.MaxAge, o.Now)
	res.StaleOwners = sweepOwners(*root)
	tmpBase := filepath.Join(o.Base, "tmp")
	res.addAll(removeOlder(tmpBase, o.Now.Add(-o.TmpAge)))
	if o.Base != root.Dir {
		res.addAll(removeOlder(root.Tmp(), o.Now.Add(-o.TmpAge)))
	}
	busy := func(dd dataDir) bool {
		if o.KeepLegacy && dd.legacy {
			return true
		}
		if o.Owned[dd.path] != nil {
			return true
		}
		if dd.current && root.OwnerHeld(dd.id) {
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
		// loose files in legacy/ (outside device dirs) expire too
		if !o.KeepLegacy {
			res.addAll(removeOlder(filepath.Join(o.Base, "legacy"), cut))
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
		loose := []string{tmpBase}
		if !o.KeepLegacy {
			loose = append(loose, filepath.Join(o.Base, "legacy"))
		}
		for _, d := range loose {
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
			if c.dd.path == "" { // loose legacy / tmp file
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
	pruneEmpty(filepath.Join(o.Base, "legacy"), true)
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

// sweepOwners removes owner lock files nobody holds (left by a crash).
func sweepOwners(r Root) int {
	ents, _ := os.ReadDir(r.Owners())
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		p := filepath.Join(r.Owners(), e.Name())
		f, ok := store.TryLock(p)
		if !ok {
			continue
		}
		os.Remove(p) // under the lock; AcquireOwner re-checks the inode
		store.ReleaseLock(f)
		n++
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
		if err != nil || strings.HasSuffix(e.Name(), ".json") {
			continue
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
