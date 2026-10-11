package store

import (
	"bytes"
	"container/list"
	"encoding/json"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// History reads (records older than the in-memory window) go through a
// per-segment block index: each segment is split into blocks of about
// BlockBytes at line boundaries, and decoded blocks are kept in an LRU cache
// capped by (estimated) bytes. A big segment therefore never has to be held
// in memory as a whole, and the cache size does not depend on segment size.

// block is a run of complete lines [off,end) of one segment file.
type block struct {
	first, last uint64 // min / max seq in the block
	off, end    int64
	n           int
	// minT / maxT: min / max device time of the block's records (issue #21).
	// Device time is not monotonic in stored order (buffers interleave, the
	// clock can be set back), so these are true bounds, not the times of the
	// first and last line. A line without a readable time makes the block
	// unbounded (math.MinInt64 / math.MaxInt64).
	minT, maxT int64
}

// outside reports whether every record of b lies outside [fromNs, toNs)
// (0: unbounded side).
func (b block) outside(fromNs, toNs int64) bool {
	return (fromNs != 0 && b.maxT < fromNs) || (toNs != 0 && b.minT >= toNs)
}

type segIndex struct {
	blocks []block
	end    int64 // bytes indexed (always at a line boundary)
}

type blockKey struct {
	path string
	off  int64
}

type cachedBlock struct {
	key   blockKey
	end   int64
	recs  []*model.Record
	bytes int64
}

// RecordBytes estimates the memory one decoded record keeps alive: the
// struct, its strings and the canonical line the filter engine caches on it
// (Record.Line). It is an upper estimate; strings shared by interning are
// counted once per record.
func RecordBytes(r *model.Record) int64 {
	return int64(200 + 2*len(r.Msg) + 2*len(r.Tag) + 2*len(r.Process) + 2*len(r.Package) + len(r.Boot) + len(r.Buffer))
}

// lineSeq extracts the seq of one stored line without a full decode (lines
// start with {"s":N,). Falls back to JSON.
func lineSeq(line []byte) (uint64, bool) {
	const p = `{"s":`
	if bytes.HasPrefix(line, []byte(p)) {
		i := len(p)
		j := i
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > i {
			if v, err := strconv.ParseUint(string(line[i:j]), 10, 64); err == nil {
				return v, true
			}
		}
	}
	var x struct {
		S uint64 `json:"s"`
	}
	if json.Unmarshal(line, &x) == nil && x.S > 0 {
		return x.S, true
	}
	return 0, false
}

// lineTime extracts the device time ("t") of one stored line without a full
// decode: lines start with {"s":N,"b":"<boot>","t":T,. Falls back to JSON.
func lineTime(line []byte) (int64, bool) {
	const key = `"t":`
	head := line[:min(len(line), 96)]
	if i := bytes.Index(head, []byte(key)); i > 0 && bytes.HasPrefix(line, []byte(`{"s":`)) {
		j := i + len(key)
		k := j
		if k < len(line) && line[k] == '-' {
			k++
		}
		for k < len(line) && line[k] >= '0' && line[k] <= '9' {
			k++
		}
		if k > j && k < len(line) && (line[k] == ',' || line[k] == '}') {
			if v, err := strconv.ParseInt(string(line[j:k]), 10, 64); err == nil {
				return v, true
			}
		}
	}
	var x struct {
		T *int64 `json:"t"`
	}
	if json.Unmarshal(line, &x) == nil && x.T != nil {
		return *x.T, true
	}
	return 0, false
}

// segIndexFor returns the (extended to the current file size) index of a
// segment, or nil if it cannot be read.
func (s *Store) segIndexFor(path string) *segIndex {
	st, err := os.Stat(path)
	s.hmu.Lock()
	defer s.hmu.Unlock()
	ix := s.idx[path]
	if err != nil {
		if ix != nil && os.IsNotExist(err) {
			s.dropPathLocked(path)
		}
		return nil
	}
	if ix == nil {
		ix = &segIndex{}
		s.idx[path] = ix
	}
	if st.Size() > ix.end {
		extendIndex(path, ix, s.cfg.BlockBytes)
	}
	return ix
}

// extendIndex reads complete lines from ix.end on and appends them to the
// index; a trailing block that is not yet full is reopened and continued.
func extendIndex(path string, ix *segIndex, blockBytes int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var cur block
	if n := len(ix.blocks); n > 0 && ix.blocks[n-1].end-ix.blocks[n-1].off < blockBytes {
		cur = ix.blocks[n-1]
		ix.blocks = ix.blocks[:n-1]
	} else {
		cur = block{off: ix.end, end: ix.end}
	}
	pos := ix.end
	buf := make([]byte, 1<<20)
	var carry []byte
	for {
		n, rerr := f.ReadAt(buf, pos)
		pos += int64(n)
		data := buf[:n]
		if len(carry) > 0 {
			data = append(carry, data...)
		}
		start := pos - int64(len(data)) // file offset of data[0]
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				break
			}
			line := data[:i]
			lineEnd := start + int64(i) + 1
			if seq, ok := lineSeq(line); ok {
				t, tok := lineTime(line)
				if !tok {
					t = math.MinInt64 // unknown: the block can never be skipped
				}
				if cur.n == 0 {
					cur.first, cur.minT, cur.maxT = seq, t, t
				}
				if seq < cur.first {
					cur.first = seq
				}
				if seq > cur.last {
					cur.last = seq
				}
				if !tok {
					cur.minT, cur.maxT = math.MinInt64, math.MaxInt64
				} else if cur.maxT != math.MaxInt64 || cur.minT != math.MinInt64 {
					cur.minT, cur.maxT = min(cur.minT, t), max(cur.maxT, t)
				}
				cur.n++
			}
			cur.end = lineEnd
			ix.end = lineEnd
			if cur.end-cur.off >= blockBytes {
				ix.blocks = append(ix.blocks, cur)
				cur = block{off: cur.end, end: cur.end}
			}
			data = data[i+1:]
			start = lineEnd
		}
		carry = append(carry[:0:0], data...)
		if rerr != nil || n == 0 {
			break
		}
	}
	if cur.end > cur.off && cur.n > 0 {
		ix.blocks = append(ix.blocks, cur)
	}
}

// decodeRange decodes the complete lines in [off,end) of a segment file.
func decodeRange(path string, off, end int64) ([]*model.Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	data := make([]byte, end-off)
	if _, err := f.ReadAt(data, off); err != nil && err != io.EOF {
		return nil, 0, err
	}
	recs, size := decodeLines(data)
	return recs, size, nil
}

// decodeLines decodes JSONL records; repeated short strings (boot id, tag,
// buffer, process and package names) are interned so the decoded records
// share them.
func decodeLines(data []byte) ([]*model.Record, int64) {
	recs := make([]*model.Record, 0, len(data)/200+1)
	in := map[string]string{}
	intern := func(v string) string {
		if len(v) > 128 {
			return v
		}
		if x, ok := in[v]; ok {
			return x
		}
		in[v] = v
		return v
	}
	var size int64
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i]
		data = data[i+1:]
		if len(line) == 0 {
			continue
		}
		r := new(model.Record)
		if DecodeRecord(line, r) != nil {
			continue
		}
		// records written before printable-unescaping existed
		r.Msg = logcat.UnescapePrintable(r.Msg)
		r.Boot, r.Tag, r.Buffer = intern(r.Boot), intern(r.Tag), intern(r.Buffer)
		r.Process, r.Package = intern(r.Process), intern(r.Package)
		size += RecordBytes(r)
		recs = append(recs, r)
	}
	return recs, size
}

// blockRecords returns the decoded records of one block, via the LRU cache.
func (s *Store) blockRecords(path string, b block) []*model.Record {
	k := blockKey{path, b.off}
	s.hmu.Lock()
	if e, ok := s.blocks[k]; ok {
		cb := e.Value.(*cachedBlock)
		if cb.end == b.end {
			s.lru.MoveToFront(e)
			s.hmu.Unlock()
			return cb.recs
		}
		s.removeBlockLocked(e) // the block grew (tail of the active segment)
	}
	s.hmu.Unlock()
	recs, size, err := decodeRange(path, b.off, b.end)
	if err != nil {
		return nil
	}
	if size > s.cfg.HistoryCacheBytes {
		return recs // too big to cache at all
	}
	s.hmu.Lock()
	defer s.hmu.Unlock()
	if _, ok := s.blocks[k]; ok { // decoded concurrently
		return recs
	}
	s.blocks[k] = s.lru.PushFront(&cachedBlock{key: k, end: b.end, recs: recs, bytes: size})
	s.hBytes += size
	for s.hBytes > s.cfg.HistoryCacheBytes && s.lru.Len() > 1 {
		s.removeBlockLocked(s.lru.Back())
	}
	return recs
}

func (s *Store) removeBlockLocked(e *list.Element) {
	cb := e.Value.(*cachedBlock)
	s.lru.Remove(e)
	delete(s.blocks, cb.key)
	s.hBytes -= cb.bytes
}

// dropPathLocked forgets the index and cached blocks of a (deleted) segment.
func (s *Store) dropPathLocked(path string) {
	delete(s.idx, path)
	for e := s.lru.Front(); e != nil; {
		next := e.Next()
		if e.Value.(*cachedBlock).key.path == path {
			s.removeBlockLocked(e)
		}
		e = next
	}
}

func (s *Store) dropCache(path string) {
	s.hmu.Lock()
	s.dropPathLocked(path)
	s.hmu.Unlock()
}

// HistoryCacheBytes is the estimated size of the decoded history cache.
func (s *Store) HistoryCacheBytes() int64 {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	return s.hBytes
}

// touch records a query and arms the idle-release timer.
func (s *Store) touch() {
	if s.cfg.IdleRelease <= 0 {
		return
	}
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.lastUse = time.Now()
	if s.idleT == nil && !s.closed.Load() {
		s.idleT = time.AfterFunc(s.cfg.IdleRelease, s.idleFire)
	}
}

func (s *Store) idleFire() {
	s.hmu.Lock()
	left := s.cfg.IdleRelease - time.Since(s.lastUse)
	if left > 0 && !s.closed.Load() {
		s.idleT = time.AfterFunc(left, s.idleFire)
		s.hmu.Unlock()
		return
	}
	s.idleT = nil
	s.hmu.Unlock()
	if s.closed.Load() {
		return
	}
	if s.ReleaseCaches() > 0 && freeOSMemory != nil {
		freeOSMemory()
	}
}

// ReleaseCaches drops the decoded history cache and, in read-only
// (follower) stores, the recent window too; both are rebuilt from disk on
// the next query. It returns the estimated bytes released.
func (s *Store) ReleaseCaches() int64 {
	s.hmu.Lock()
	freed := s.hBytes
	s.blocks = map[blockKey]*list.Element{}
	s.lru.Init()
	s.hBytes = 0
	s.hmu.Unlock()
	s.mu.Lock()
	if !s.writer && s.loaded {
		freed += s.memBytes
		s.mem, s.memBytes, s.loaded = nil, 0, false
	}
	s.mu.Unlock()
	return freed
}
