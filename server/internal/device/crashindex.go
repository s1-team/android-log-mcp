package device

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// crashIndex is the shared in-memory index of one device's crashes.jsonl
// (issue #22). It holds a small summary of the newest version of each
// crash (no stack, traces or DropBox text) and the file offset of that
// version, so:
//   - the recorder and tool readers no longer re-read and decode the whole
//     file for every crash and every call: only bytes appended since the
//     last read are decoded;
//   - the full crash is decoded on demand from its offset;
//   - a rewrite of the file (CompactAux, or another process compacting)
//     is detected (different file, shorter file, or changed bytes before
//     the indexed end) and the index is rebuilt.
//
// One index per crashes.jsonl path, shared by every Store of that device
// directory in this process.
type crashIndex struct {
	mu     sync.Mutex
	path   string
	fi     os.FileInfo // the file indexed (nil: none yet)
	off    int64       // bytes indexed, always at a line boundary
	tail   []byte      // the last bytes before off (rewrite check)
	ents   map[string]*crashEnt
	order  []string          // ids in order of first appearance
	sorted []*analysis.Crash // cached summaries, newest first (nil: rebuild)
}

type crashEnt struct {
	sum *analysis.Crash // summary: heavy text fields empty
	off int64           // newest version: byte offset and length (without '\n')
	n   int
}

// crashSummary is the subset of analysis.Crash kept in the index: the
// fields list views, matching and grouping use. Body, Traces, CPU, Dropbox
// and Tombstone are decoded only on demand.
type crashSummary struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Boot      string   `json:"boot"`
	TimeNs    int64    `json:"t"`
	Seq       uint64   `json:"seq,omitempty"`
	PID       int      `json:"pid"`
	Process   string   `json:"process,omitempty"`
	Package   string   `json:"package,omitempty"`
	Thread    string   `json:"thread,omitempty"`
	Headline  string   `json:"headline"`
	Signature string   `json:"sig,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Location  string   `json:"location,omitempty"`
}

const crashTailBytes = 64

var crashIndexes sync.Map // abs path of crashes.jsonl → *crashIndex

// crashLinesIndexed counts crashes.jsonl lines decoded into summaries
// (tests: ingest must stay linear in the number of crashes).
var crashLinesIndexed atomic.Int64

func crashIndexOf(st *store.Store) *crashIndex {
	p := filepath.Join(st.Dir, "crashes.jsonl")
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if x, ok := crashIndexes.Load(p); ok {
		return x.(*crashIndex)
	}
	x, _ := crashIndexes.LoadOrStore(p, &crashIndex{path: p})
	return x.(*crashIndex)
}

func (x *crashIndex) resetLocked() {
	x.fi, x.off, x.tail = nil, 0, nil
	x.ents, x.order, x.sorted = map[string]*crashEnt{}, nil, nil
}

// refreshLocked brings the index up to date and returns the open file
// (nil when there is none); the caller closes it.
func (x *crashIndex) refreshLocked() *os.File {
	if x.ents == nil {
		x.resetLocked()
	}
	f, err := os.Open(x.path)
	if err != nil {
		if x.fi != nil || x.off > 0 {
			x.resetLocked()
		}
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil
	}
	if x.fi != nil && (!os.SameFile(x.fi, fi) || fi.Size() < x.off || !x.tailMatches(f)) {
		x.resetLocked() // rewritten (compaction) or replaced: rebuild
	}
	x.fi = fi
	if fi.Size() > x.off {
		buf := make([]byte, fi.Size()-x.off)
		n, _ := f.ReadAt(buf, x.off)
		buf = buf[:n]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			x.addLocked(buf[:i+1], x.off)
			x.off += int64(i + 1)
			x.tail = append(x.tail[:0], buf[max(0, i+1-crashTailBytes):i+1]...)
			if len(x.tail) < crashTailBytes && x.off > int64(len(x.tail)) {
				x.tail = x.readTail(f)
			}
		}
	}
	return f
}

func (x *crashIndex) readTail(f *os.File) []byte {
	n := min(int64(crashTailBytes), x.off)
	b := make([]byte, n)
	if m, _ := f.ReadAt(b, x.off-n); int64(m) != n {
		return nil
	}
	return b
}

func (x *crashIndex) tailMatches(f *os.File) bool {
	if x.off == 0 {
		return true
	}
	b := x.readTail(f)
	return b != nil && bytes.Equal(b, x.tail)
}

// addLocked indexes complete lines (data ends with '\n') starting at base.
func (x *crashIndex) addLocked(data []byte, base int64) {
	pos := 0
	for pos < len(data) {
		i := bytes.IndexByte(data[pos:], '\n')
		line := data[pos : pos+i]
		crashLinesIndexed.Add(1)
		var s crashSummary
		if len(line) > 0 && json.Unmarshal(line, &s) == nil && s.ID != "" {
			c := &analysis.Crash{ID: s.ID, Kind: s.Kind, Boot: s.Boot, TimeNs: s.TimeNs, Seq: s.Seq, PID: s.PID,
				Process: s.Process, Package: s.Package, Thread: s.Thread, Headline: s.Headline, Signature: s.Signature,
				Sources: s.Sources[:len(s.Sources):len(s.Sources)], Location: s.Location} // full slice: appends by callers copy
			e := x.ents[s.ID]
			if e == nil {
				e = &crashEnt{}
				x.ents[s.ID] = e
				x.order = append(x.order, s.ID)
			}
			e.sum, e.off, e.n = c, base+int64(pos), len(line)
			x.sorted = nil
		}
		pos += i + 1
	}
}

func (x *crashIndex) summariesLocked() []*analysis.Crash {
	if x.sorted == nil {
		out := make([]*analysis.Crash, 0, len(x.order))
		for _, id := range x.order {
			out = append(out, x.ents[id].sum)
		}
		// same order as before the index: first appearance, then newest
		// device time first (stable)
		sort.SliceStable(out, func(i, j int) bool { return out[i].TimeNs > out[j].TimeNs })
		x.sorted = out
	}
	return x.sorted
}

// summaries returns the crash summaries, newest first. The slice and the
// crashes are shared: callers must not modify them.
func (x *crashIndex) summaries() []*analysis.Crash {
	x.mu.Lock()
	defer x.mu.Unlock()
	if f := x.refreshLocked(); f != nil {
		f.Close()
	}
	return x.summariesLocked()
}

// full decodes the newest version of the given crashes (nil entries for
// ids no longer present). The returned crashes are the caller's.
func (x *crashIndex) full(ids ...string) []*analysis.Crash {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*analysis.Crash, len(ids))
	f := x.refreshLocked()
	if f == nil {
		return out
	}
	defer f.Close()
	var buf []byte
	for i, id := range ids {
		e := x.ents[id]
		if e == nil {
			continue
		}
		if cap(buf) < e.n {
			buf = make([]byte, e.n)
		}
		b := buf[:e.n]
		if _, err := f.ReadAt(b, e.off); err != nil && err != io.EOF {
			continue
		}
		c := new(analysis.Crash)
		if json.Unmarshal(b, c) == nil && c.ID == id {
			out[i] = c
		}
	}
	return out
}

// CrashSummaries returns the device's crashes (newest version of each,
// newest first) without their stack, traces and DropBox text; use
// CrashFull for those. Shared: callers must not modify the crashes.
func CrashSummaries(st *store.Store) []*analysis.Crash {
	return crashIndexOf(st).summaries()
}

// CrashFull decodes the newest version of each given crash id in full
// (nil for ids that are not stored).
func CrashFull(st *store.Store, ids ...string) []*analysis.Crash {
	return crashIndexOf(st).full(ids...)
}

// LoadCrashes reads all crashes in full (newest version of each ID, newest
// first). Prefer CrashSummaries + CrashFull for list views.
func LoadCrashes(st *store.Store) []*analysis.Crash {
	x := crashIndexOf(st)
	x.mu.Lock()
	var ids []string
	f := x.refreshLocked()
	if f != nil {
		f.Close()
	}
	for _, c := range x.summariesLocked() {
		ids = append(ids, c.ID)
	}
	x.mu.Unlock()
	out := x.full(ids...)
	n := 0
	for _, c := range out {
		if c != nil {
			out[n] = c
			n++
		}
	}
	return out[:n]
}
