// Package store keeps per-device logcat records in append-only JSONL segment
// files under <base>/<serial>/ plus a bounded in-memory window of recent
// records (records + bytes caps). Older records are read from disk in blocks
// through a byte-capped LRU (history.go); caches are released when idle.
//
// One process per device directory is the writer (holds writer.lock); other
// processes open the same directory read-only as followers and tail the files.
// In production only the service process writes; followers are used by
// --offline and tests.
package store

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Config controls sizes and retention.
type Config struct {
	SegmentBytes int64         // rotate segments at this size (default 16 MiB)
	MaxBytes     int64         // per-device cap (default 512 MiB)
	MaxAge       time.Duration // retention (default 1 day; ANDROID_LOG_MCP_MAX_AGE)
	MemRecords   int           // recent in-memory window, records (default 20k)
	MemBytes     int64         // recent window, estimated bytes (default 16 MiB)
	// HistoryCacheBytes caps the decoded blocks of older records kept for
	// history queries (default 32 MiB).
	HistoryCacheBytes int64
	// IdleRelease drops the history cache (and a follower's recent window)
	// after this long without queries (default 2 min; negative: never).
	IdleRelease time.Duration
	BlockBytes  int64 // history read/cache granularity (default 512 KiB)
}

// Defaults (see README "环境变量与数据目录").
const (
	DefaultMemRecords        = 20_000
	DefaultMemBytes          = 16 << 20
	DefaultHistoryCacheBytes = 32 << 20
	DefaultIdleRelease       = 2 * time.Minute
	DefaultMaxBytes          = 512 << 20
	DefaultMaxAge            = 24 * time.Hour
)

func (c *Config) defaults() {
	if c.SegmentBytes <= 0 {
		c.SegmentBytes = 16 << 20
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxAge <= 0 {
		c.MaxAge = DefaultMaxAge
	}
	if c.MemRecords <= 0 {
		c.MemRecords = DefaultMemRecords
	}
	if c.MemBytes <= 0 {
		c.MemBytes = DefaultMemBytes
	}
	if c.HistoryCacheBytes <= 0 {
		c.HistoryCacheBytes = DefaultHistoryCacheBytes
	}
	if c.IdleRelease == 0 {
		c.IdleRelease = DefaultIdleRelease
	}
	if c.BlockBytes <= 0 {
		c.BlockBytes = 512 << 10
	}
}

// freeOSMemory returns released cache memory to the OS after an idle
// release (nil in tests that only check accounting).
var freeOSMemory = debug.FreeOSMemory

type segment struct {
	first uint64
	path  string
	size  int64
}

// Store is one device's log store.
type Store struct {
	Dir    string
	Serial string
	cfg    Config

	mu       sync.RWMutex
	writer   bool
	lockF    *os.File
	mem      []*model.Record // ascending seq, contiguous
	memBytes int64           // RecordBytes sum of mem
	loaded   bool            // mem holds the recent window (followers load it on demand)
	lastSeq  uint64
	newestNs int64 // time of the newest record
	segs     []segment
	cur      *os.File
	curW     *bufio.Writer
	curSize  int64
	curStart int64 // host time the active segment was started (0 = unknown); never device time (#48)
	meta     Meta
	metaMod  time.Time // mtime and size of the meta.json last read: a follower reloads when either differs
	metaSize int64
	dirty    bool
	notify   chan struct{}
	repair   *segRepair // a failed write left bytes to cut before the next write (issues #182, #191)

	// follower tail state
	tailSeg string
	tailOff int64

	// history: block index per segment + byte-capped LRU of decoded blocks
	hmu     sync.Mutex
	idx     map[string]*segIndex
	blocks  map[blockKey]*list.Element
	lru     *list.List
	hBytes  int64
	classes classDict // per-store class dictionary for block counts (issue #37)
	lastUse time.Time
	idleT   *time.Timer
	closed  atomic.Bool

	auxMu sync.Mutex // serializes aux appends with compaction
}

// segRepair says a segment must be cut back to size bytes (its last
// durable size) before anything is appended to it again.
type segRepair struct {
	path string
	size int64
}

func newStore(dir string, cfg Config) *Store {
	return &Store{Dir: dir, cfg: cfg, notify: make(chan struct{}),
		idx: map[string]*segIndex{}, blocks: map[blockKey]*list.Element{}, lru: list.New()}
}

// SanitizeSerial makes a serial safe as a directory name.
func SanitizeSerial(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "_%02x", r)
		}
	}
	return b.String()
}

// Open opens (creating if needed) the store for device serial (its ID) in
// base/v<DataFormat>/<serial>. It becomes the writer if it can take the
// lock, else a follower.
func Open(base, serial string, cfg Config) (*Store, error) {
	return OpenDir(filepath.Join(DataDir(base), SanitizeSerial(serial)), serial, cfg, true)
}

// OpenDir opens (creating if needed) the store in dir for device serial (the
// device ID). With tryWriter it becomes the writer if it can take the
// directory lock; otherwise it is a follower until TryPromote.
func OpenDir(dir, serial string, cfg Config, tryWriter bool) (*Store, error) {
	cfg.defaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := newStore(dir, cfg)
	s.Serial = serial
	s.meta.Serial = serial
	s.loadMeta()
	if s.meta.Serial != "" && s.meta.Serial != serial && serial != "" {
		s.meta.Serial = serial
	}
	if tryWriter {
		if f, ok := tryLock(filepath.Join(dir, "writer.lock")); ok {
			s.writer = true
			s.lockF = f
		}
	}
	if err := s.loadSegments(); err != nil {
		return nil, err
	}
	if s.writer {
		s.prune()
	}
	return s, nil
}

// OpenReadOnly opens an existing device directory without ever writing. The
// recent window is loaded on the first query.
func OpenReadOnly(dir string, cfg Config) (*Store, error) {
	cfg.defaults()
	s := newStore(dir, cfg)
	s.loadMeta()
	s.Serial = s.meta.Serial
	if err := s.loadSegments(); err != nil {
		return nil, err
	}
	return s, nil
}

// IsWriter reports whether this process owns the device directory.
func (s *Store) IsWriter() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writer
}

// TryPromote attempts to become the writer (e.g. after the previous writer exited).
func (s *Store) TryPromote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer {
		return true
	}
	f, ok := tryLock(filepath.Join(s.Dir, "writer.lock"))
	if !ok {
		return false
	}
	// The old writer is gone: take over what it left on disk exactly as
	// OpenDir does for a writer (re-list the segments, cut a torn last
	// line, reload the window and the newest seq), before the first
	// Append. Refresh does nothing for a writer, so a follower that only
	// switched the flag would reuse seqs the old writer had already
	// written and append to its partial last line (issue #180).
	segs, err := s.listSegments()
	if err != nil {
		f.Close()
		return false
	}
	s.writer = true
	s.lockF = f
	s.metaMod = time.Time{} // meta.json is reloaded whatever its mtime
	s.loadMetaLocked()
	s.tailSeg, s.tailOff = "", 0
	if err := s.loadSegmentsLocked(segs, true); err != nil {
		// stay a follower: give the lock back so a later TryPromote retries
		s.writer = false
		s.releaseLockLocked()
		return false
	}
	close(s.notify)
	s.notify = make(chan struct{})
	return true
}

func (s *Store) loadMeta() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadMetaLocked()
}

func (s *Store) loadMetaLocked() {
	p := filepath.Join(s.Dir, "meta.json")
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	// Not "newer": a host clock set back (or a coarse timestamp) can stamp
	// a newer file with an older or equal time (issue #183).
	if !s.metaMod.IsZero() && st.ModTime().Equal(s.metaMod) && st.Size() == s.metaSize {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m Meta
	if json.Unmarshal(b, &m) == nil {
		serial := s.meta.Serial
		s.meta = m
		if s.meta.Serial == "" {
			s.meta.Serial = serial
		}
		s.metaMod, s.metaSize = st.ModTime(), st.Size()
		s.lastSeq = max(s.lastSeq, m.SeqFloor)
	}
}

// Meta returns a copy of the metadata.
func (s *Store) Meta() Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.meta
	m.Sessions = append([]Session(nil), m.Sessions...)
	m.Gaps = append([]Gap(nil), m.Gaps...)
	m.Holes = append([]SeqRange(nil), m.Holes...)
	m.ClockJumps = append([]ClockJump(nil), m.ClockJumps...)
	m.SegTimes = append([]SegTime(nil), m.SegTimes...)
	return m
}

// UpdateMeta mutates metadata (writer only) and persists it.
func (s *Store) UpdateMeta(fn func(m *Meta)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.meta)
	if s.writer {
		s.saveMetaLocked()
	}
}

func (s *Store) saveMetaLocked() {
	// A failed save stays dirty: Flush retries it (issue #187).
	s.dirty = true
	b, _ := json.MarshalIndent(&s.meta, "", " ")
	tmp := filepath.Join(s.Dir, "meta.json.tmp")
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	// On Windows the rename fails while another process (a follower, a
	// report) has meta.json open; stay dirty and retry on the next Flush.
	if os.Rename(tmp, filepath.Join(s.Dir, "meta.json")) != nil {
		return
	}
	s.dirty = false
}

func segName(first uint64) string { return fmt.Sprintf("seg-%020d.jsonl", first) }

func (s *Store) listSegments() ([]segment, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var segs []segment
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "seg-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		first, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(n, "seg-"), ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segment{first: first, path: filepath.Join(s.Dir, n), size: info.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	return segs, nil
}

// loadSegments lists segment files and positions the tail reader. The
// writer fills the recent window right away (it keeps appending to it);
// followers only learn the newest seq and load the window on demand.
func (s *Store) loadSegments() error {
	segs, err := s.listSegments()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadSegmentsLocked(segs, false)
}

// loadSegmentsLocked is loadSegments for the listed segments, with s.mu held.
// With strict, a torn last line that cannot be cut is an error (a promoted
// follower must not write behind it); OpenDir keeps going as before.
func (s *Store) loadSegmentsLocked(segs []segment, strict bool) error {
	s.segs = segs
	if len(segs) == 0 {
		s.loaded = true
		return nil
	}
	if s.writer {
		// Repair a torn last line left by a crash.
		last := segs[len(segs)-1]
		if err := truncateTorn(last.path); err == nil {
			if st, err := os.Stat(last.path); err == nil {
				s.segs[len(s.segs)-1].size = st.Size()
			}
		} else if strict {
			return err
		}
		s.loadWindowLocked()
		return nil
	}
	s.findTailLocked()
	return nil
}

// findTailLocked sets lastSeq / newestNs / the tail position from the last
// complete line on disk without decoding anything else.
func (s *Store) findTailLocked() {
	n := len(s.segs)
	if n == 0 {
		return
	}
	s.tailSeg, s.tailOff = s.segs[n-1].path, 0
	for i := n - 1; i >= 0; i-- {
		r, end := lastRecord(s.segs[i].path)
		if i == n-1 {
			s.tailOff = end
		}
		if r != nil {
			s.lastSeq = max(s.lastSeq, r.Seq)
			s.newestNs = r.TimeNs
			return
		}
		if s.segs[i].first > 0 {
			s.lastSeq = max(s.lastSeq, s.segs[i].first-1)
		}
	}
}

// loadWindowLocked fills the recent window (newest MemRecords / MemBytes)
// from disk, block by block from the end.
func (s *Store) loadWindowLocked() {
	var chunks [][]*model.Record
	count, size := 0, int64(0)
	n := len(s.segs)
	for i := n - 1; i >= 0 && count < s.cfg.MemRecords && size < s.cfg.MemBytes; i-- {
		ix := s.segIndexFor(s.segs[i].path)
		if ix == nil {
			continue
		}
		if i == n-1 {
			s.tailSeg, s.tailOff = s.segs[i].path, ix.end
		}
		for b := len(ix.blocks) - 1; b >= 0 && count < s.cfg.MemRecords && size < s.cfg.MemBytes; b-- {
			recs, sz, err := decodeRange(s.segs[i].path, ix.blocks[b].off, ix.blocks[b].end)
			if err != nil {
				continue
			}
			chunks = append(chunks, recs)
			count += len(recs)
			size += sz
		}
	}
	mem := make([]*model.Record, 0, count)
	for i := len(chunks) - 1; i >= 0; i-- {
		mem = append(mem, chunks[i]...)
	}
	mem = contiguousTail(mem)
	s.mem, s.memBytes = mem, sumBytes(mem)
	s.trimLocked(true)
	if len(s.mem) > 0 {
		last := s.mem[len(s.mem)-1]
		s.lastSeq = max(s.lastSeq, last.Seq)
		s.newestNs = last.TimeNs
	} else {
		s.findTailLocked()
	}
	s.loaded = true
}

func sumBytes(m []*model.Record) int64 {
	var n int64
	for _, r := range m {
		n += RecordBytes(r)
	}
	return n
}

// trimLocked drops the oldest window records beyond MemRecords / MemBytes.
// Unless exact, it only trims once a cap is exceeded by 25% so appends do
// not copy the window every time.
func (s *Store) trimLocked(exact bool) {
	maxN, maxB := s.cfg.MemRecords, s.cfg.MemBytes
	if !exact && len(s.mem) <= maxN+maxN/4 && s.memBytes <= maxB+maxB/4 {
		return
	}
	drop := 0
	b := s.memBytes
	for drop < len(s.mem) && (len(s.mem)-drop > maxN || b > maxB) {
		b -= RecordBytes(s.mem[drop])
		drop++
	}
	if drop == 0 {
		return
	}
	s.mem = append([]*model.Record(nil), s.mem[drop:]...)
	s.memBytes = b
}

// lastRecord decodes the last complete line of a segment and returns it with
// the offset just past it (nil, 0 if the file has no complete line).
func lastRecord(path string) (*model.Record, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil, 0
	}
	const chunk = 64 << 10
	var tail []byte // bytes from pos to the end of the file
	end := int64(-1)
	for pos := st.Size(); pos > 0; {
		n := int64(chunk)
		if pos < n {
			n = pos
		}
		pos -= n
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, pos); err != nil && err != io.EOF {
			return nil, 0
		}
		tail = append(buf, tail...)
		if end < 0 {
			i := bytes.LastIndexByte(tail, '\n')
			if i < 0 {
				continue
			}
			end = pos + int64(i) + 1
		}
		body := tail[:end-pos-1] // up to (excluding) the final newline
		if j := bytes.LastIndexByte(body, '\n'); j >= 0 || pos == 0 {
			line := body[j+1:]
			r := new(model.Record)
			if DecodeRecord(line, r) != nil {
				return nil, end
			}
			r.Msg = logcat.UnescapePrintable(r.Msg)
			return r, end
		}
	}
	return nil, max(end, 0)
}

// contiguousTail keeps the longest suffix whose seqs increase by exactly 1.
func contiguousTail(m []*model.Record) []*model.Record {
	for i := len(m) - 1; i > 0; i-- {
		if m[i].Seq != m[i-1].Seq+1 {
			return m[i:]
		}
	}
	return m
}

func truncateTorn(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	// scan back for the last newline
	const chunk = 64 << 10
	end := st.Size()
	buf := make([]byte, chunk)
	for pos := end; pos > 0; {
		n := int64(chunk)
		if pos < n {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			if pos+int64(i)+1 != end {
				return f.Truncate(pos + int64(i) + 1)
			}
			return nil
		}
	}
	return f.Truncate(0)
}

// readSegment decodes complete lines from offset; returns records and the
// offset just past the last complete line.
func readSegment(path string, off int64) ([]*model.Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, off, err
	}
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		return nil, off, nil
	}
	recs, _ := decodeLines(data[:last+1])
	return recs, off + int64(last) + 1, nil
}

// LastSeq is the newest sequence number (0 = empty).
func (s *Store) LastSeq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeq
}

// FirstSeq is the oldest sequence still stored (0 = empty).
func (s *Store) FirstSeq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.segs) > 0 {
		return max(s.segs[0].first, 1)
	}
	if len(s.mem) > 0 {
		return s.mem[0].Seq
	}
	return 0
}

// Append assigns sequence numbers and persists records (writer only).
func (s *Store) Append(recs []*model.Record) error {
	if len(recs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writer {
		return errors.New("store: not the writer")
	}
	if err := s.repairLocked(); err != nil {
		return err
	}
	// Append is all or nothing: lastSeq, the window and the meta times are
	// published only once the batch is flushed. On an error the batch is
	// rolled back (failAppendLocked), so the caller can write it again
	// without burning seqs or leaving records that only memory has
	// (issues #182, #191).
	mk := appendMark{lastSeq: s.lastSeq, mem: s.mem, memBytes: s.memBytes, newestNs: s.newestNs,
		arrival: s.meta.ArrivalTimeNs, lastT: s.meta.LastTimeNs, curStart: s.curStart}
	if n := len(s.segs); n > 0 {
		mk.path, mk.size = s.segs[n-1].path, s.segs[n-1].size
	}
	nowHost := hostNow().UnixNano()
	for _, r := range recs {
		s.lastSeq++
		r.Seq = s.lastSeq
		if s.cur == nil || s.curSize >= s.cfg.SegmentBytes || s.segmentTooOld(nowHost) {
			if err := s.rotateLocked(r.Seq); err != nil {
				return s.failAppendLocked(mk, err)
			}
		}
		b, err := json.Marshal(r)
		if err != nil {
			return s.failAppendLocked(mk, err)
		}
		b = append(b, '\n')
		if s.curSize == 0 {
			s.curStart = nowHost
		}
		n, err := s.curW.Write(b)
		s.curSize += int64(n)
		if err != nil {
			return s.failAppendLocked(mk, err)
		}
		if n := len(s.mem); n > 0 && s.mem[n-1].Seq+1 != r.Seq {
			s.mem, s.memBytes = nil, 0 // seq jump (set-root move): the window must stay contiguous
		}
		s.mem = append(s.mem, r)
		s.memBytes += RecordBytes(r)
		s.newestNs = r.TimeNs
		s.meta.ArrivalTimeNs = r.TimeNs
		if r.TimeNs > s.meta.LastTimeNs {
			s.meta.LastTimeNs = r.TimeNs
		}
	}
	// Make every batch visible to follower processes right away (the
	// recorder appends ~every 100 ms; meta.json is saved by Flush, ~2/s).
	if err := s.curW.Flush(); err != nil {
		return s.failAppendLocked(mk, err)
	}
	s.segs[len(s.segs)-1].size = s.curSize
	if n := len(s.meta.SegTimes); n > 0 && s.meta.SegTimes[n-1].First == s.segs[len(s.segs)-1].first {
		s.meta.SegTimes[n-1].EndNs = nowHost
	}
	s.meta.IngestHostNs = time.Now().UnixNano()
	s.trimLocked(false)
	s.dirty = true
	close(s.notify)
	s.notify = make(chan struct{})
	return nil
}

// appendMark is the state Append restores when a batch fails.
type appendMark struct {
	lastSeq        uint64
	mem            []*model.Record
	memBytes       int64
	newestNs       int64
	arrival, lastT int64
	curStart       int64
	path           string // last segment before the batch, and its durable size
	size           int64
}

// failAppendLocked undoes a failed batch: segments it created are removed,
// the segment it continued is cut back to its durable size, and the
// writer is closed so the next Append opens it afresh. When the cut
// itself fails it is retried by repairLocked before the next write.
func (s *Store) failAppendLocked(mk appendMark, err error) error {
	if s.cur != nil {
		s.cur.Close()
	}
	s.cur, s.curW, s.curSize = nil, nil, 0
	kept := s.segs[:0]
	for _, sg := range s.segs {
		if sg.first > mk.lastSeq {
			os.Remove(sg.path)
			s.dropCache(sg.path)
			continue
		}
		kept = append(kept, sg)
	}
	s.segs = kept
	st := s.meta.SegTimes[:0]
	for _, t := range s.meta.SegTimes {
		if t.First <= mk.lastSeq {
			st = append(st, t)
		}
	}
	s.meta.SegTimes = st
	for i := range s.segs {
		if s.segs[i].path == mk.path {
			s.segs[i].size = mk.size // a rotation in the batch had set the larger size
		}
	}
	s.lastSeq, s.mem, s.memBytes, s.newestNs = mk.lastSeq, mk.mem, mk.memBytes, mk.newestNs
	s.meta.ArrivalTimeNs, s.meta.LastTimeNs, s.curStart = mk.arrival, mk.lastT, mk.curStart
	if mk.path != "" {
		s.repair = &segRepair{mk.path, mk.size}
		s.repairLocked()
	}
	return err
}

// repairLocked cuts a segment back after a failed write (see segRepair).
func (s *Store) repairLocked() error {
	r := s.repair
	if r == nil {
		return nil
	}
	if err := os.Truncate(r.path, r.size); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: cannot repair %s after a failed write: %w", filepath.Base(r.path), err)
	}
	s.repair = nil
	// the history index and cached blocks may cover bytes that are gone
	// (a query between the flush and the cut): drop them, they are rebuilt
	// from the file
	s.dropCache(r.path)
	return nil
}

// continuesSeg reports whether seq first directly follows segment sg.
func continuesSeg(sg segment, first uint64) bool {
	if r, _ := lastRecord(sg.path); r != nil {
		return r.Seq+1 == first
	}
	return sg.first == first
}

func (s *Store) rotateLocked(first uint64) error {
	if s.cur != nil {
		if err := s.curW.Flush(); err != nil {
			return err // the batch is rolled back by Append
		}
		s.cur.Close()
		s.cur = nil
		if n := len(s.segs); n > 0 {
			s.segs[n-1].size = s.curSize
		}
	}
	nowH := hostNow().UnixNano()
	s.ensureStampsLocked(nowH)
	// Reuse the last segment if it is not full (e.g. after restart) and the
	// seqs continue it (not after a set-root move, which leaves a seq jump).
	if n := len(s.segs); n > 0 && s.segs[n-1].size < s.cfg.SegmentBytes && s.cur == nil && s.curSize == 0 && continuesSeg(s.segs[n-1], first) {
		f, err := os.OpenFile(s.segs[n-1].path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			s.cur, s.curW, s.curSize = f, bufio.NewWriterSize(f, 256<<10), s.segs[n-1].size
			s.curStart = s.stampFor(s.segs[n-1].first).StartNs
			if s.curSize < s.cfg.SegmentBytes && !s.segmentTooOld(nowH) {
				return nil
			}
			f.Close()
			s.cur = nil
		}
	}
	p := filepath.Join(s.Dir, segName(first))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.cur, s.curW, s.curSize = f, bufio.NewWriterSize(f, 256<<10), 0
	s.segs = append(s.segs, segment{first: first, path: p})
	s.meta.SegTimes = append(s.meta.SegTimes, SegTime{First: first, StartNs: nowH, EndNs: nowH})
	s.dirty = true
	s.pruneLocked()
	return nil
}

// Flush writes buffered data and metadata (writer only). Call periodically.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writer {
		return
	}
	if s.curW != nil {
		if err := s.curW.Flush(); err != nil {
			// buffered bytes (none after a successful Append) could not be
			// written: drop the writer and cut the segment back to what is
			// durable
			s.cur.Close()
			s.cur, s.curW, s.curSize = nil, nil, 0
			if n := len(s.segs); n > 0 {
				s.repair = &segRepair{s.segs[n-1].path, s.segs[n-1].size}
				s.repairLocked()
			}
		}
	}
	if s.dirty {
		s.meta.LastSeenHostNs = time.Now().UnixNano()
		s.saveMetaLocked()
	}
	if runtime.GOOS == "windows" && s.lockF != nil {
		now := time.Now()
		os.Chtimes(s.lockF.Name(), now, now)
	}
}

// Closed reports whether Close was called. A closed read-only store still
// serves reads (they open the segment files per call).
func (s *Store) Closed() bool { return s.closed.Load() }

// Close flushes and releases the writer lock.
func (s *Store) Close() {
	s.Flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	// the writer stops here, so nothing retries a failed meta.json save
	// later (on Windows a follower may hold the file open for a moment)
	for i := 0; s.writer && s.dirty && i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		s.saveMetaLocked()
	}
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
	s.releaseLockLocked()
	s.writer = false
	s.closed.Store(true)
	s.hmu.Lock()
	if s.idleT != nil {
		s.idleT.Stop()
		s.idleT = nil
	}
	s.hmu.Unlock()
}

// releaseLockLocked releases writer.lock (s.mu held).
func (s *Store) releaseLockLocked() {
	if s.lockF == nil {
		return
	}
	name := s.lockF.Name()
	s.lockF.Close()
	if runtime.GOOS == "windows" {
		os.Remove(name)
	}
	s.lockF = nil
}

// segmentTooOld (nowNs: host time): the active segment already spans a quarter of the
// retention window, so start a new one. Retention deletes whole segments
// once their last write (file mtime, host time) is older than MaxAge; capping the span keeps quiet
// devices (that never fill a segment) from keeping logs for much longer
// than MaxAge (at most ~1.25 × MaxAge).
func (s *Store) segmentTooOld(nowNs int64) bool {
	return s.curStart > 0 && nowNs-s.curStart > int64(s.cfg.MaxAge/4)
}

// Prune applies the size / age retention now (writer only). The recorder
// also prunes on every segment rotation; this catches quiet periods.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer {
		s.pruneLocked()
	}
}

func (s *Store) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
}

// pruneLocked deletes whole oldest segments beyond MaxBytes / MaxAge.
// The active (last) segment is never deleted.
func (s *Store) pruneLocked() {
	var total int64
	for _, sg := range s.segs {
		total += sg.size
	}
	now := hostNow().UnixNano()
	s.ensureStampsLocked(now)
	cutoff := now - int64(s.cfg.MaxAge)
	for len(s.segs) > 1 {
		old := s.segs[0]
		tooOld := s.stampFor(old.first).EndNs < cutoff
		if total <= s.cfg.MaxBytes && !tooOld {
			break
		}
		// On Windows a segment a follower is reading right now cannot be
		// deleted; keep it and retry on the next rotation instead of
		// forgetting it on disk.
		if err := os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			break
		}
		total -= old.size
		s.segs = s.segs[1:]
		s.dropCache(old.path)
	}
	s.dropMemBeforeLocked()
}

// dropMemBeforeLocked removes window records older than the oldest segment.
func (s *Store) dropMemBeforeLocked() {
	if len(s.segs) > 0 && len(s.mem) > 0 && s.mem[0].Seq < s.segs[0].first {
		k := s.segs[0].first - s.mem[0].Seq
		if k >= uint64(len(s.mem)) {
			s.mem, s.memBytes = nil, 0
		} else {
			s.memBytes -= sumBytes(s.mem[:k])
			s.mem = append([]*model.Record(nil), s.mem[k:]...)
		}
	}
}

// RemoveOldestSegment deletes the oldest segment if it is path and not the
// active one (writer only; used by the data-directory size cap).
func (s *Store) RemoveOldestSegment(path string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writer || len(s.segs) < 2 || s.segs[0].path != path {
		return 0, false
	}
	old := s.segs[0]
	if err := os.Remove(old.path); err != nil && !os.IsNotExist(err) {
		return 0, false
	}
	s.segs = s.segs[1:]
	s.dropCache(old.path)
	s.dropMemBeforeLocked()
	return old.size, true
}

// Refresh (followers) picks up records appended by the writer process.
func (s *Store) Refresh() {
	s.mu.RLock()
	w := s.writer
	s.mu.RUnlock()
	if w {
		return
	}
	segs, err := s.listSegments()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadMetaLocked()
	s.segs = segs
	if len(segs) == 0 {
		return
	}
	var added bool
	start := -1
	for i, sg := range segs {
		if sg.path == s.tailSeg {
			start = i
			break
		}
	}
	if start < 0 {
		start = 0
		for i, sg := range segs {
			if sg.first <= s.lastSeq+1 {
				start = i
			}
		}
	}
	for i := start; i < len(segs); i++ {
		off := int64(0)
		if segs[i].path == s.tailSeg {
			off = s.tailOff
		}
		recs, end, err := readSegment(segs[i].path, off)
		if err != nil {
			continue
		}
		s.tailSeg, s.tailOff = segs[i].path, end
		for _, r := range recs {
			if r.Seq <= s.lastSeq {
				continue
			}
			if s.loaded { // unloaded followers only track the newest seq
				if len(s.mem) > 0 && r.Seq != s.lastSeq+1 {
					s.mem, s.memBytes = nil, 0 // discontinuity: restart window
				}
				s.mem = append(s.mem, r)
				s.memBytes += RecordBytes(r)
			}
			s.lastSeq = r.Seq
			s.newestNs = r.TimeNs
			added = true
		}
	}
	s.dropMemBeforeLocked()
	s.trimLocked(false)
	if added {
		close(s.notify)
		s.notify = make(chan struct{})
	}
}

// Wait blocks until a record newer than afterSeq exists, ctx ends, or timeout.
func (s *Store) Wait(ctx context.Context, afterSeq uint64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.Refresh()
		s.mu.RLock()
		last, ch, w := s.lastSeq, s.notify, s.writer
		s.mu.RUnlock()
		if last > afterSeq {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		step := left
		if !w && step > 300*time.Millisecond {
			step = 300 * time.Millisecond
		}
		t := time.NewTimer(step)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

// Scan visits records with after < seq < before (before==0: no upper bound),
// ascending or descending. fn returns false to stop. Records older than the
// memory window are read from disk.
func (s *Store) Scan(after, before uint64, desc bool, fn func(*model.Record) bool) {
	s.ScanTime(after, before, desc, 0, 0, fn)
}

// AddClockJump records a device clock jump (writer only). A jump at the
// same Seq as the newest recorded one (the same jump seen again, e.g. a
// clock reading before the first line after it) is merged: the first
// report wins, a "possible" one is upgraded.
// sameJump: a line-based delta (which includes the gap between the two
// lines) describes the measured one: within 60 s + 10 %. Several jumps
// while disconnected add up to a measured delta that no single one
// matches; it then stays at the session's first line.
func sameJump(measured, lines int64) bool {
	d := lines - measured
	if d < 0 {
		d = -d
	}
	if measured < 0 {
		measured = -measured
	}
	return d <= ClockJumpThreshold+measured/10
}

// measuredRefineSeqs: how far after a "measured" clock jump (a clock
// read at reconnect, placed at the session's first line) a line-based
// report of the same jump may move it.
const measuredRefineSeqs = 1 << 20

func (s *Store) AddClockJump(j ClockJump) {
	s.UpdateMeta(func(m *Meta) {
		if n := len(m.ClockJumps); n > 0 {
			last := &m.ClockJumps[n-1]
			if last.Seq >= j.Seq {
				if last.Source == "possible" && j.Source != "possible" && last.Seq == j.Seq {
					*last = j
				}
				return
			}
			// a clock reading names the next seq, but lines logged before
			// the jump may still have been queued: the line-based report
			// of the same jump a little later gives the exact seq
			// (a replay of up to measuredRefineSeqs lines); the clock
			// reading's delta stays, the source becomes the line-based one
			if last.Source == "measured" && j.Source != "measured" && j.Seq-last.Seq <= measuredRefineSeqs && (last.DeltaNs < 0) == (j.DeltaNs < 0) && sameJump(last.DeltaNs, j.DeltaNs) {
				last.Seq, last.Source = j.Seq, j.Source
				if j.BeforeNs != 0 {
					last.BeforeNs, last.AfterNs = j.BeforeNs, j.AfterNs
				}
				return
			}
		}
		m.ClockJumps = append(m.ClockJumps, j)
		if len(m.ClockJumps) > maxClockJumps {
			m.ClockJumps = append([]ClockJump(nil), m.ClockJumps[len(m.ClockJumps)-maxClockJumps:]...)
		}
	})
}

// ClockJumps returns the recorded device clock jumps whose lines are still
// stored, oldest first.
func (s *Store) ClockJumps() []ClockJump {
	m := s.Meta()
	first := s.FirstSeq()
	var out []ClockJump
	for _, j := range m.ClockJumps {
		if j.Seq >= first && j.Seq <= s.LastSeq()+1 {
			out = append(out, j)
		}
	}
	return out
}

// ScanTime is Scan restricted to records whose device time is in
// [fromNs, toNs) (0: unbounded side), in the same seq order (issue #21).
// History blocks whose time range lies wholly outside the window are not
// read. It never stops on record order: device time is not monotonic in
// stored order (the clock can be set back), so every record in the window
// is visited however it is stored (issue #48).
func (s *Store) ScanTime(after, before uint64, desc bool, fromNs, toNs int64, fn func(*model.Record) bool) {
	s.scanTime(after, before, desc, fromNs, toNs, nil, fn)
}

// ScanTimeClass is ScanTime with a countable class-id set: blocks whose
// classes do not intersect ids are skipped without decoding (issue #37).
func (s *Store) ScanTimeClass(after, before uint64, desc bool, fromNs, toNs int64, ids map[uint32]bool, fn func(*model.Record) bool) {
	s.scanTime(after, before, desc, fromNs, toNs, ids, fn)
}

func (s *Store) scanTime(after, before uint64, desc bool, fromNs, toNs int64, ids map[uint32]bool, fn func(*model.Record) bool) {
	if fromNs != 0 || toNs != 0 {
		inner := fn
		fn = func(r *model.Record) bool {
			if (fromNs != 0 && r.TimeNs < fromNs) || (toNs != 0 && r.TimeNs >= toNs) {
				return true
			}
			return inner(r)
		}
	}
	s.touch()
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if !loaded { // followers load the recent window on the first query
		s.mu.Lock()
		if !s.loaded {
			s.loadWindowLocked()
		}
		s.mu.Unlock()
	}
	s.mu.RLock()
	mem := s.mem
	segs := append([]segment(nil), s.segs...)
	last := s.lastSeq
	s.mu.RUnlock()
	if before == 0 || before > last+1 {
		before = last + 1
	}
	if after >= before-1 { // before >= 1 here; avoids after+1 wrapping at MaxUint64
		return
	}
	memFirst := uint64(0)
	if len(mem) > 0 {
		memFirst = mem[0].Seq
	}
	inMem := func(lo, hi uint64, desc bool) bool { // [lo,hi)
		// seqs above the window's last one exist nowhere here (e.g. a
		// set-root move back left meta.seq_floor above the stored data)
		hi = min(hi, memFirst+uint64(len(mem)))
		if len(mem) == 0 || hi <= memFirst || lo >= hi {
			return true
		}
		lo = max(lo, memFirst)
		if desc {
			for q := hi - 1; q >= lo; q-- {
				if !fn(mem[q-memFirst]) {
					return false
				}
				if q == 0 {
					break
				}
			}
			return true
		}
		for q := lo; q < hi; q++ {
			if !fn(mem[q-memFirst]) {
				return false
			}
		}
		return true
	}
	onDisk := func(lo, hi uint64, desc bool) bool { // [lo,hi), only seqs < memFirst
		if len(mem) > 0 {
			hi = min(hi, memFirst)
		}
		if lo >= hi {
			return true
		}
		idx := make([]int, 0)
		for i, sg := range segs {
			end := uint64(1<<63 - 1)
			if i+1 < len(segs) {
				end = segs[i+1].first
			}
			if sg.first < hi && end > lo {
				idx = append(idx, i)
			}
		}
		if desc {
			for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
				idx[i], idx[j] = idx[j], idx[i]
			}
		}
		for _, i := range idx {
			ix := s.segIndexFor(segs[i].path)
			if ix == nil {
				continue
			}
			var bl []block
			for _, b := range ix.blocks {
				if b.first < hi && b.last >= lo && !b.outside(fromNs, toNs) && b.classMatch(ids) {
					bl = append(bl, b)
				}
			}
			if desc {
				for k := len(bl) - 1; k >= 0; k-- {
					recs := s.blockRecords(segs[i].path, bl[k])
					for j := len(recs) - 1; j >= 0; j-- {
						r := recs[j]
						if r.Seq >= lo && r.Seq < hi && !fn(r) {
							return false
						}
					}
				}
				continue
			}
			for _, b := range bl {
				for _, r := range s.blockRecords(segs[i].path, b) {
					if r.Seq >= lo && r.Seq < hi && !fn(r) {
						return false
					}
				}
			}
		}
		return true
	}
	lo, hi := after+1, before
	if desc {
		if inMem(lo, hi, true) {
			onDisk(lo, hi, true)
		}
	} else {
		if onDisk(lo, hi, false) {
			inMem(lo, hi, false)
		}
	}
}

// Count returns the number of records with after < seq < before
// (before==0: no upper bound), exactly what Scan would visit, without
// decoding them (issue #20): the in-memory window is contiguous, and older
// records are counted from the per-segment block index; only blocks that
// straddle the range bounds are decoded.
func (s *Store) Count(after, before uint64) uint64 {
	s.touch()
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if !loaded {
		s.mu.Lock()
		if !s.loaded {
			s.loadWindowLocked()
		}
		s.mu.Unlock()
	}
	s.mu.RLock()
	mem := s.mem
	segs := append([]segment(nil), s.segs...)
	last := s.lastSeq
	s.mu.RUnlock()
	if before == 0 || before > last+1 {
		before = last + 1
	}
	if after >= before-1 { // before >= 1 here; avoids after+1 wrapping at MaxUint64
		return 0
	}
	lo, hi := after+1, before
	var n uint64
	memFirst := uint64(0)
	if len(mem) > 0 {
		memFirst = mem[0].Seq
		if mlo, mhi := max(lo, memFirst), min(hi, memFirst+uint64(len(mem))); mhi > mlo {
			n += mhi - mlo
		}
		hi = min(hi, memFirst) // the rest is on disk
	}
	if lo >= hi {
		return n
	}
	for i, sg := range segs {
		end := uint64(1<<63 - 1)
		if i+1 < len(segs) {
			end = segs[i+1].first
		}
		if sg.first >= hi || end <= lo {
			continue
		}
		ix := s.segIndexFor(sg.path)
		if ix == nil {
			continue
		}
		for _, b := range ix.blocks {
			switch {
			case b.n == 0 || b.first >= hi || b.last < lo:
			case b.first >= lo && b.last < hi:
				n += uint64(b.n)
			default: // straddles a bound: count its records
				for _, r := range s.blockRecords(sg.path, b) {
					if r.Seq >= lo && r.Seq < hi {
						n++
					}
				}
			}
		}
	}
	return n
}

// CountMatch returns the number of records with after < seq < before that
// match a countable class filter (ids) and lie in [fromNs, toNs) (0 =
// unbounded). Blocks wholly inside the window contribute their class sums;
// straddling, uncounted, or jump-straddling blocks are decoded. The
// in-memory window is always scanned. ids==nil means no class filter (every
// class counts, as in ScanTimeClass), so match must accept every record then.
// Issue #37, #188.
func (s *Store) CountMatch(after, before uint64, fromNs, toNs int64, ids map[uint32]bool, match func(*model.Record) bool) uint64 {
	s.touch()
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if !loaded {
		s.mu.Lock()
		if !s.loaded {
			s.loadWindowLocked()
		}
		s.mu.Unlock()
	}
	s.mu.RLock()
	mem := s.mem
	segs := append([]segment(nil), s.segs...)
	last := s.lastSeq
	s.mu.RUnlock()
	if before == 0 || before > last+1 {
		before = last + 1
	}
	if after >= before-1 { // before >= 1 here; avoids after+1 wrapping at MaxUint64
		return 0
	}
	lo, hi := after+1, before
	var n uint64
	memFirst := uint64(0)
	inTime := func(r *model.Record) bool {
		return (fromNs == 0 || r.TimeNs >= fromNs) && (toNs == 0 || r.TimeNs < toNs)
	}
	if len(mem) > 0 {
		memFirst = mem[0].Seq
		if mlo, mhi := max(lo, memFirst), min(hi, memFirst+uint64(len(mem))); mhi > mlo {
			for q := mlo; q < mhi; q++ {
				r := mem[q-memFirst]
				if inTime(r) && match(r) {
					n++
				}
			}
		}
		hi = min(hi, memFirst)
	}
	if lo >= hi {
		return n
	}
	for i, sg := range segs {
		end := uint64(1<<63 - 1)
		if i+1 < len(segs) {
			end = segs[i+1].first
		}
		if sg.first >= hi || end <= lo {
			continue
		}
		ix := s.segIndexFor(sg.path)
		if ix == nil {
			continue
		}
		for _, b := range ix.blocks {
			if b.n == 0 || b.first >= hi || b.last < lo || b.outside(fromNs, toNs) {
				continue
			}
			// wholly inside the seq range and the time window: use counts
			whollySeq := b.first >= lo && b.last < hi
			if whollySeq && b.whollyInside(fromNs, toNs) {
				if sum, ok := b.classSum(ids); ok {
					n += sum
					continue
				}
			}
			// straddling, uncounted, or unreadable time: decode
			for _, r := range s.blockRecords(sg.path, b) {
				if r.Seq >= lo && r.Seq < hi && inTime(r) && match(r) {
					n++
				}
			}
		}
	}
	return n
}

// Get returns the record with the given seq, or nil.
func (s *Store) Get(seq uint64) *model.Record {
	var out *model.Record
	s.Scan(seq-1, seq+1, false, func(r *model.Record) bool { out = r; return false })
	return out
}

// Stats summarizes stored data.
type Stats struct {
	Records    uint64
	Bytes      int64
	OldestNs   int64
	NewestNs   int64
	FirstSeq   uint64
	LastSeq    uint64
	MemRecords int
	MemBytes   int64 // estimated size of the recent window
	MemLoaded  bool  // false: follower whose window is not loaded (idle)
	CacheBytes int64 // estimated size of the history block cache
}

func (s *Store) Stats() Stats {
	s.mu.RLock()
	st := Stats{LastSeq: s.lastSeq, MemRecords: len(s.mem), MemBytes: s.memBytes, MemLoaded: s.loaded, NewestNs: s.newestNs}
	for _, sg := range s.segs {
		st.Bytes += sg.size
	}
	first := uint64(0)
	if len(s.segs) > 0 {
		first = max(s.segs[0].first, 1)
	} else if len(s.mem) > 0 {
		first = s.mem[0].Seq
	}
	holes := s.meta.Holes
	s.mu.RUnlock()
	st.FirstSeq = first
	if first > 0 && st.LastSeq >= first {
		st.Records = st.LastSeq - first + 1
		for _, h := range holes { // seqs recorded in another root (set-root)
			lo, hi := max(h.First, first), min(h.Last, st.LastSeq)
			if hi >= lo && st.Records > hi-lo {
				st.Records -= hi - lo + 1
			}
		}
		st.OldestNs = s.oldestNs()
	}
	st.CacheBytes = s.HistoryCacheBytes()
	return st
}

// ---- auxiliary JSONL files (crashes, processes, dropbox cache) ----

// AppendAux appends one JSON value to <dir>/<name>.jsonl (writer only).
func (s *Store) AppendAux(name string, v any) error {
	if !s.IsWriter() {
		return errors.New("store: not the writer")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.auxMu.Lock()
	defer s.auxMu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.Dir, name+".jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// A crash (or a short write) can leave a partial last line. Start on a
	// fresh line, or the new entry would join it and be lost (issue #181);
	// readers skip the undecodable partial line.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadAux returns all complete lines of <dir>/<name>.jsonl.
func (s *Store) ReadAux(name string) [][]byte {
	b, err := os.ReadFile(filepath.Join(s.Dir, name+".jsonl"))
	if err != nil {
		return nil
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	} else {
		return nil
	}
	return bytes.Split(b, []byte{'\n'})
}

// RewriteAux atomically replaces an aux file (writer only), e.g. to trim it.
func (s *Store) RewriteAux(name string, lines [][]byte) error {
	if !s.IsWriter() {
		return errors.New("store: not the writer")
	}
	s.auxMu.Lock()
	defer s.auxMu.Unlock()
	return s.rewriteAuxLocked(name, lines)
}

func (s *Store) rewriteAuxLocked(name string, lines [][]byte) error {
	tmp := filepath.Join(s.Dir, name+".jsonl.tmp")
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		os.Remove(tmp) // a partial file (disk full) would keep eating space
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.Dir, name+".jsonl")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// DataFormat is the on-disk format version: device data lives in
// <root>/v<DataFormat>/<device-id>/ (also inside reports: store/v1/<id>/).
// Directories of versions before 0.4 (<root>/<serial>/) are ignored.
const DataFormat = 1

// DataDir is <base>/v<DataFormat>.
func DataDir(base string) string { return filepath.Join(base, fmt.Sprintf("v%d", DataFormat)) }

// ListDeviceDirs lists the device directories (<base>/v1/<id> with a
// meta.json). Anything else under base, e.g. pre-0.4 <base>/<serial>
// directories, is ignored.
func ListDeviceDirs(base string) []string { return listMetaDirs(DataDir(base)) }

func listMetaDirs(base string) []string {
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(base, e.Name(), "meta.json")); err == nil {
				out = append(out, filepath.Join(base, e.Name()))
			}
		}
	}
	return out
}

// SegmentFile is one log segment on disk.
type SegmentFile struct {
	Path  string
	First uint64
	Size  int64
}

// ListSegments lists the segment files of a device directory, oldest first.
func ListSegments(dir string) []SegmentFile {
	segs, _ := (&Store{Dir: dir}).listSegments()
	out := make([]SegmentFile, len(segs))
	for i, s := range segs {
		out[i] = SegmentFile{Path: s.path, First: s.first, Size: s.size}
	}
	return out
}

// TryLock takes an exclusive, non-blocking lock file (flock on unix,
// LockFileEx on Windows, an exclusively created file elsewhere); it is held
// while the file stays open and released by the OS when the process exits.
func TryLock(path string) (*os.File, bool) { return tryLock(path) }

// RefreshLock keeps a held lock fresh; only the fallback lock of platforms
// with neither flock nor LockFileEx needs it (it goes stale after a minute).
func RefreshLock(path string) { refreshLock(path) }

// ReleaseLock releases a lock taken with TryLock.
func ReleaseLock(f *os.File) { releaseLock(f) }

// LockHeld reports whether someone currently holds the lock file at path.
func LockHeld(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	f, ok := tryLock(path)
	if !ok {
		return true
	}
	releaseLock(f)
	return false
}

// Dir-level helpers for callers that manage several stores.

// WriterLockHeld reports whether a process writes the device directory dir.
func WriterLockHeld(dir string) bool { return LockHeld(filepath.Join(dir, "writer.lock")) }

// Config returns the effective configuration (defaults applied).
func (s *Store) Config() Config { return s.cfg }

// oldestNs is the time of the oldest stored record, read from the first
// line of the oldest segment (no cache involved).
func (s *Store) oldestNs() int64 {
	s.mu.RLock()
	var path string
	if len(s.segs) > 0 {
		path = s.segs[0].path
	}
	var memT int64
	if len(s.mem) > 0 {
		memT = s.mem[0].TimeNs
	}
	s.mu.RUnlock()
	if path != "" {
		if t := firstRecordNs(path); t != 0 {
			return t
		}
	}
	return memT
}

// hostNow is the host clock for segment rotation (tests replace it).
var hostNow = time.Now

// staleHostSpan is how old a segment's last host-time stamp may be before
// the stamp is distrusted: a host clock that was far off while recording
// (RTC not set, dead battery) and was then corrected makes every stamp look
// ancient, and deleting by it would silently drop recent data (issue #184).
func (s *Store) staleHostSpan() int64 {
	return max(int64(30*24*time.Hour), 10*int64(s.cfg.MaxAge))
}

// stampFor returns the stamp of the segment with this first seq (a copy; zero
// value when unknown, which ensureStampsLocked prevents for stored segments).
func (s *Store) stampFor(first uint64) SegTime {
	for _, t := range s.meta.SegTimes {
		if t.First == first {
			return t
		}
	}
	return SegTime{}
}

// ensureStampsLocked makes s.meta.SegTimes describe exactly the stored
// segments with sane host times (writer only):
//   - a segment without a stamp (a store written before the stamps existed)
//     gets one from the file mtimes, as before;
//   - a stamp in the future (the host clock was ahead and was corrected) is
//     set to now, so the segment ages from the correction (issue #189);
//   - a stamp older than staleHostSpan is distrusted and set to now, so
//     nothing is deleted at once after a clock correction, and the segment
//     still ages out one MaxAge later (issue #184);
//   - stamps of segments that are gone are dropped.
func (s *Store) ensureStampsLocked(now int64) {
	if !s.writer {
		return
	}
	old := s.meta.SegTimes
	out := make([]SegTime, 0, len(s.segs))
	changed := len(old) != len(s.segs)
	for i, sg := range s.segs {
		var t SegTime
		found := false
		for _, o := range old {
			if o.First == sg.first {
				t, found = o, true
				break
			}
		}
		if !found {
			t = SegTime{First: sg.first}
			if fi, err := os.Stat(sg.path); err == nil {
				t.EndNs = fi.ModTime().UnixNano()
				t.StartNs = t.EndNs
			}
			if i > 0 {
				if fi, err := os.Stat(s.segs[i-1].path); err == nil {
					t.StartNs = fi.ModTime().UnixNano()
				}
			}
			changed = true
		}
		n := t
		if n.EndNs == 0 && n.StartNs == 0 || n.StartNs > now || n.EndNs > now || now-n.EndNs > s.staleHostSpan() {
			n.StartNs, n.EndNs = now, now
		}
		if n.StartNs > n.EndNs {
			n.StartNs = n.EndNs
		}
		if n != t {
			changed = true
		}
		out = append(out, n)
	}
	if changed {
		s.meta.SegTimes = out
		s.dirty = true
	}
}

// firstRecordNs is the time of the first complete line of a segment (0 if none).
func firstRecordNs(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return 0
	}
	var r model.Record
	if DecodeRecord(line, &r) != nil {
		return 0
	}
	return r.TimeNs
}
