package device

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// launchIndex is the shared in-memory index of one device's launches.jsonl
// (issue #270 / #173 G3). Parallel to crashIndex: summaries in RAM, byte
// offset for full decode, append-only refresh with SameFile/tail rewrite
// detection. Format version is per-line (analysis.LaunchFormatVer); a
// mismatch triggers rebuild via EnsureLaunches.
type launchIndex struct {
	mu     sync.Mutex
	path   string
	fi     os.FileInfo
	off    int64
	tail   []byte
	ents   map[string]*launchEnt
	order  []string
	sorted []*LaunchSum // newest first (nil: rebuild)
	verBad bool         // saw a line with unexpected ver
}

type launchEnt struct {
	sum *LaunchSum
	off int64
	n   int
}

// LaunchSum is the subset kept in the index for list filters (#270).
type LaunchSum struct {
	ID        string
	Package   string
	Component string
	Class     string
	Kind      string
	RequestNs int64
	Seq       uint64
	Boot      string
	PID       int
	DispMs    int64 // Displayed.TookMs; -1 if no Displayed
	ClockJump bool  // stored value; query recomputes via ApplyLaunchJumps
}

const launchTailBytes = 64

var launchIndexes sync.Map // abs path → *launchIndex

var launchLinesIndexed atomic.Int64

// launchAuxLine is one launches.jsonl record: full Launch plus id/ver/ht/boot.
type launchAuxLine struct {
	*analysis.Launch
	ID     string `json:"id"`
	Ver    int    `json:"ver"`
	HostNs int64  `json:"ht,omitempty"`
	Boot   string `json:"boot,omitempty"`
}

func launchIndexOf(st *store.Store) *launchIndex {
	p := filepath.Join(st.Dir, "launches.jsonl")
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if x, ok := launchIndexes.Load(p); ok {
		return x.(*launchIndex)
	}
	x, _ := launchIndexes.LoadOrStore(p, &launchIndex{path: p})
	return x.(*launchIndex)
}

func (x *launchIndex) resetLocked() {
	x.fi, x.off, x.tail = nil, 0, nil
	x.ents, x.order, x.sorted = map[string]*launchEnt{}, nil, nil
	x.verBad = false
}

var openLaunches = os.Open
var launchOpenTries = 40

func openLaunchRetry(path string) (*os.File, error) {
	var f *os.File
	var err error
	for i := 0; i < launchOpenTries; i++ {
		if f, err = openLaunches(path); err == nil || os.IsNotExist(err) {
			return f, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, err
}

func (x *launchIndex) refreshLocked() *os.File {
	if x.ents == nil {
		x.resetLocked()
	}
	f, err := openLaunchRetry(x.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil
		}
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
		x.resetLocked()
	}
	x.fi = fi
	if fi.Size() > x.off {
		buf := make([]byte, fi.Size()-x.off)
		n, _ := f.ReadAt(buf, x.off)
		buf = buf[:n]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			x.addLocked(buf[:i+1], x.off)
			x.off += int64(i + 1)
			x.tail = append(x.tail[:0], buf[max(0, i+1-launchTailBytes):i+1]...)
			if len(x.tail) < launchTailBytes && x.off > int64(len(x.tail)) {
				x.tail = x.readTail(f)
			}
		}
	}
	return f
}

func (x *launchIndex) readTail(f *os.File) []byte {
	n := min(int64(launchTailBytes), x.off)
	b := make([]byte, n)
	if m, _ := f.ReadAt(b, x.off-n); int64(m) != n {
		return nil
	}
	return b
}

func (x *launchIndex) tailMatches(f *os.File) bool {
	if x.off == 0 {
		return true
	}
	b := x.readTail(f)
	return b != nil && bytes.Equal(b, x.tail)
}

func (x *launchIndex) addLocked(data []byte, base int64) {
	pos := 0
	for pos < len(data) {
		i := bytes.IndexByte(data[pos:], '\n')
		line := data[pos : pos+i]
		launchLinesIndexed.Add(1)
		var s struct {
			ID        string `json:"id"`
			Ver       int    `json:"ver"`
			Boot      string `json:"boot"`
			Package   string `json:"package"`
			Component string `json:"component"`
			Class     string `json:"class"`
			Kind      string `json:"kind"`
			RequestNs int64  `json:"t"`
			Seq       uint64 `json:"seq"`
			PID       int    `json:"pid"`
			ClockJump bool   `json:"clock_jumped"`
			Displayed *struct {
				TookMs int64 `json:"took_ms"`
				Ns     int64 `json:"ns"`
			} `json:"displayed"`
		}
		if len(line) > 0 && json.Unmarshal(line, &s) == nil && s.ID != "" {
			if s.Ver != analysis.LaunchFormatVer {
				x.verBad = true
			} else {
				disp := int64(-1)
				if s.Displayed != nil && s.Displayed.Ns != 0 {
					disp = s.Displayed.TookMs
				}
				sum := &LaunchSum{
					ID: s.ID, Package: s.Package, Component: s.Component, Class: s.Class,
					Kind: s.Kind, RequestNs: s.RequestNs, Seq: s.Seq, Boot: s.Boot, PID: s.PID,
					DispMs: disp, ClockJump: s.ClockJump,
				}
				e := x.ents[s.ID]
				if e == nil {
					e = &launchEnt{}
					x.ents[s.ID] = e
					x.order = append(x.order, s.ID)
				}
				e.sum, e.off, e.n = sum, base+int64(pos), len(line)
				x.sorted = nil
			}
		}
		pos += i + 1
	}
}

func (x *launchIndex) summariesLocked() []*LaunchSum {
	if x.sorted == nil {
		out := make([]*LaunchSum, 0, len(x.order))
		for i := len(x.order) - 1; i >= 0; i-- {
			out = append(out, x.ents[x.order[i]].sum)
		}
		x.sorted = out
	}
	return x.sorted
}

func (x *launchIndex) summaries() []*LaunchSum {
	x.mu.Lock()
	defer x.mu.Unlock()
	if f := x.refreshLocked(); f != nil {
		f.Close()
	}
	return x.summariesLocked()
}

func (x *launchIndex) full(ids ...string) []*analysis.Launch {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*analysis.Launch, len(ids))
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
		l := launchAuxLine{Launch: new(analysis.Launch)}
		if json.Unmarshal(b, &l) == nil && l.ID == id && l.Launch != nil {
			out[i] = l.Launch
		}
	}
	return out
}

func (x *launchIndex) needsRebuild() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	f := x.refreshLocked()
	if f != nil {
		f.Close()
	}
	if x.verBad {
		return true
	}
	// missing file
	if _, err := os.Stat(x.path); os.IsNotExist(err) {
		return true
	}
	return false
}

// LaunchSummaries returns launch summaries (newest first). Shared.
func LaunchSummaries(st *store.Store) []*LaunchSum {
	return launchIndexOf(st).summaries()
}

// LaunchFull decodes the newest version of each launch id (nil if missing).
func LaunchFull(st *store.Store, ids ...string) []*analysis.Launch {
	return launchIndexOf(st).full(ids...)
}

// LaunchIndexReady reports that launches.jsonl exists, matches ver, and is indexed.
func LaunchIndexReady(st *store.Store) bool {
	x := launchIndexOf(st)
	x.mu.Lock()
	defer x.mu.Unlock()
	f := x.refreshLocked()
	if f != nil {
		f.Close()
	}
	if x.verBad {
		return false
	}
	_, err := os.Stat(x.path)
	return err == nil
}

// EnsureLaunches backfills launches.jsonl when absent or ver-mismatched
// (lazy, writer only; #270). incomplete is true when the scan budget ran out.
// note is a short agent-facing reason when something is incomplete.
func EnsureLaunches(st *store.Store, budget time.Duration) (incomplete bool, note string) {
	if LaunchIndexReady(st) {
		return false, ""
	}
	if !st.IsWriter() {
		// Follower: silent ScanTime fallback until the writer has built the file.
		return true, ""
	}
	x := launchIndexOf(st)
	x.mu.Lock()
	path := x.path
	verBad := false
	f := x.refreshLocked()
	if f != nil {
		f.Close()
	}
	verBad = x.verBad
	x.mu.Unlock()
	if verBad {
		os.Remove(path) // rebuild
		x.mu.Lock()
		x.resetLocked()
		x.mu.Unlock()
	}
	return backfillLaunches(st, budget)
}

func backfillLaunches(st *store.Store, budget time.Duration) (incomplete bool, note string) {
	if budget <= 0 {
		budget = 5 * time.Second
	}
	start := time.Now()
	var recs []*model.Record
	stopped := false
	st.ScanTime(0, 0, true, 0, 0, func(r *model.Record) bool {
		if time.Since(start) > budget {
			stopped = true
			return false
		}
		if analysis.LaunchTag(r) {
			recs = append(recs, r)
		}
		return true
	})
	jumps := st.ClockJumps()
	steps := make([]analysis.ClockStep, len(jumps))
	for i, j := range jumps {
		steps[i] = analysis.ClockStep{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}
	ls := analysis.LaunchesAcross(recs, "", steps)
	// assign stable ids by request seq order
	type keyed struct {
		seq  uint64
		l    *analysis.Launch
		boot string
	}
	var keys []keyed
	for _, l := range ls {
		boot := ""
		for _, r := range recs {
			if r.Seq == l.Seq {
				boot = r.Boot
				break
			}
		}
		keys = append(keys, keyed{l.Seq, l, boot})
	}
	// write all versions once (sealed as of scan)
	for _, k := range keys {
		var id string
		st.UpdateMeta(func(m *store.Meta) {
			m.LaunchSeq++
			id = "ln" + strconv.Itoa(m.LaunchSeq)
		})
		normalizeLaunchSteps(k.l)
		_ = st.AppendAux("launches", launchAuxLine{
			Launch: k.l, ID: id, Ver: analysis.LaunchFormatVer,
			HostNs: time.Now().UnixNano(), Boot: k.boot,
		})
	}
	// refresh index
	launchIndexOf(st).summaries()
	if stopped {
		return true, "launches_backfill"
	}
	return false, ""
}

// normalizeLaunchSteps clears TookMs on empty steps so JSON stays small.
func normalizeLaunchSteps(l *analysis.Launch) {
	if l == nil {
		return
	}
	for _, s := range []*analysis.Step{&l.ProcStart, &l.OnCreate, &l.OnRestart, &l.OnStart, &l.OnResume, &l.Displayed, &l.FullyDrawn} {
		if s.Ns == 0 {
			*s = analysis.Step{}
		}
	}
}

// launchClockSteps converts store jumps for analysis.
func launchClockSteps(st *store.Store) []analysis.ClockStep {
	jumps := st.ClockJumps()
	steps := make([]analysis.ClockStep, len(jumps))
	for i, j := range jumps {
		steps[i] = analysis.ClockStep{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}
	return steps
}
