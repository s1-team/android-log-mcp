package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
