package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// FreeSpaceFunc reports the bytes available to this user on the file
// system holding path. Tests inject fakes.
type FreeSpaceFunc func(path string) (uint64, error)

// FreeSpace is the platform implementation (statfs / GetDiskFreeSpaceEx).
var FreeSpace FreeSpaceFunc = freeSpace

var errNoFreeSpace = errors.New("free space check not supported on this platform")

// DiskGuard pauses recording while free disk space is low, with hysteresis:
// it pauses below MinFree and resumes once at least ResumeFree is available.
type DiskGuard struct {
	MinFree    uint64 // 0 disables the guard
	ResumeFree uint64 // default 2 × MinFree
	Free       FreeSpaceFunc

	mu     sync.Mutex
	paused bool
	free   uint64
	known  bool
}

// Check measures free space at path and updates the paused state. It
// returns the state and whether it changed. If free space cannot be
// measured the state is left as it was.
func (g *DiskGuard) Check(path string) (paused, changed bool) {
	if g == nil || g.MinFree == 0 {
		return false, false
	}
	fn := g.Free
	if fn == nil {
		fn = FreeSpace
	}
	free, err := fn(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		return g.paused, false
	}
	g.free, g.known = free, true
	resume := g.ResumeFree
	if resume < g.MinFree {
		resume = 2 * g.MinFree
	}
	was := g.paused
	switch {
	case !g.paused && free < g.MinFree:
		g.paused = true
	case g.paused && free >= resume:
		g.paused = false
	}
	return g.paused, g.paused != was
}

// Paused reports the last checked state.
func (g *DiskGuard) Paused() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Reason describes the pause for status output ("" when not paused).
func (g *DiskGuard) Reason() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		return ""
	}
	resume := g.ResumeFree
	if resume < g.MinFree {
		resume = 2 * g.MinFree
	}
	return fmt.Sprintf("recording paused: only %s free on the data disk (< %s; ANDROID_LOG_MCP_MIN_FREE_MB); resumes automatically at %s free",
		mib(g.free), mib(g.MinFree), mib(resume))
}

func mib(n uint64) string { return fmt.Sprintf("%d MiB", n>>20) }

// ---- data-directory total size cap ----

type segFile struct {
	dir, path string
	size      int64
	mod       time.Time
}

// DirBytes is the total size of all files in the device directories under base.
func DirBytes(base string) int64 {
	var total int64
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
			continue
		}
		sub, _ := os.ReadDir(filepath.Join(base, e.Name()))
		for _, f := range sub {
			if info, err := f.Info(); err == nil && !f.IsDir() {
				total += info.Size()
			}
		}
	}
	return total
}

// EnforceTotal deletes the oldest log segments across all device
// directories under base until the directory is at most maxTotal bytes. A
// directory recorded by this process is pruned through its store (owned);
// one with no live writer is pruned under its writer lock; directories
// recorded by another process are left to that process. The newest segment
// of each device is never deleted. Returns the bytes freed.
func EnforceTotal(base string, maxTotal int64, owned map[string]*Store) int64 {
	if maxTotal <= 0 {
		return 0
	}
	total := DirBytes(base)
	if total <= maxTotal {
		return 0
	}
	var cands []segFile
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		tmp := &Store{Dir: dir}
		segs, err := tmp.listSegments()
		if err != nil || len(segs) < 2 {
			continue
		}
		for _, sg := range segs[:len(segs)-1] {
			st, err := os.Stat(sg.path)
			if err != nil {
				continue
			}
			cands = append(cands, segFile{dir: dir, path: sg.path, size: sg.size, mod: st.ModTime()})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].mod.Before(cands[j].mod) })
	var freed int64
	skip := map[string]bool{} // a dir whose oldest segment could not be removed: keep order
	for _, c := range cands {
		if total <= maxTotal {
			break
		}
		if skip[c.dir] {
			continue
		}
		if st := owned[c.dir]; st != nil && st.IsWriter() {
			if n, ok := st.RemoveOldestSegment(c.path); ok {
				total -= n
				freed += n
				continue
			}
			skip[c.dir] = true
			continue
		}
		f, ok := tryLock(filepath.Join(c.dir, "writer.lock"))
		if !ok {
			skip[c.dir] = true // recorded by another process
			continue
		}
		err := os.Remove(c.path)
		releaseLock(f)
		if err != nil && !os.IsNotExist(err) {
			skip[c.dir] = true
			continue
		}
		total -= c.size
		freed += c.size
	}
	return freed
}

func releaseLock(f *os.File) {
	name := f.Name()
	f.Close()
	if runtime.GOOS == "windows" {
		os.Remove(name)
	}
}

// ---- auxiliary file maintenance ----

// CleanTmp removes leftover *.tmp files (interrupted atomic rewrites) older
// than age from the device directory (writer only). Returns how many.
func (s *Store) CleanTmp(age time.Duration) int {
	if !s.IsWriter() {
		return 0
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > age {
			if os.Remove(filepath.Join(s.Dir, e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// CompactAux rewrites <name>.jsonl with fn(lines) when that makes it
// smaller (writer only). Appends wait while it runs, so none are lost.
// Returns the file size before and after.
func (s *Store) CompactAux(name string, fn func(lines [][]byte) [][]byte) (before, after int64, err error) {
	if !s.IsWriter() {
		return 0, 0, errors.New("store: not the writer")
	}
	s.auxMu.Lock()
	defer s.auxMu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.Dir, name+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	before = int64(len(b))
	var lines [][]byte
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		lines = bytes.Split(b[:i], []byte{'\n'})
	}
	out := fn(lines)
	for _, l := range out {
		after += int64(len(l)) + 1
	}
	if after >= before {
		return before, before, nil
	}
	if err := s.rewriteAuxLocked(name, out); err != nil {
		return before, before, err
	}
	return before, after, nil
}
