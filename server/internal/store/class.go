package store

import (
	"bytes"
	"sort"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// MaxBlockClasses is the per-block class-count cap (issue #37). A block with
// more distinct classes is marked uncounted and decoded when counting, so
// the result stays exact.
const MaxBlockClasses = 4096

// classCount is one (class id, count) pair in a block's sorted class list.
type classCount struct {
	id, n uint32
}

// classKey is the countable identity of a record: the fields a filter on
// tag / package / process / level terms can see (issue #37). uid is part of the
// key so a shared-uid package match stays countable; there is no uid filter term
// (the key `uid:` is an alias for `package`).
type classKey struct {
	pkg, proc, tag string
	uid            int
	level          model.Level
}

// classDict maps class keys to dense uint32 ids for one Store. Protected by
// Store.hmu. Strings are interned so equal values share memory.
type classDict struct {
	keys   []classKey          // id → key; id 0 is the first class
	of     map[classKey]uint32 // key → id
	intern map[string]string
}

func (d *classDict) ensure() {
	if d.of == nil {
		d.of = map[classKey]uint32{}
		d.intern = map[string]string{}
	}
}

func (d *classDict) str(s string) string {
	if x, ok := d.intern[s]; ok {
		return x
	}
	d.intern[s] = s
	return s
}

// id returns the id of k, allocating a new one when needed.
func (d *classDict) id(k classKey) uint32 {
	d.ensure()
	k.pkg, k.proc, k.tag = d.str(k.pkg), d.str(k.proc), d.str(k.tag)
	if id, ok := d.of[k]; ok {
		return id
	}
	id := uint32(len(d.keys))
	d.keys = append(d.keys, k)
	d.of[k] = id
	return id
}

// Key returns the class for id, or the zero key when out of range.
func (d *classDict) Key(id uint32) classKey {
	if int(id) >= len(d.keys) {
		return classKey{}
	}
	return d.keys[id]
}

// Len is the number of distinct classes.
func (d *classDict) Len() int { return len(d.keys) }

// Record synthesizes a Record with only the class fields set, for evaluating
// a countable filter once per dictionary class.
func (k classKey) Record() *model.Record {
	return &model.Record{Package: k.pkg, Process: k.proc, UID: k.uid, Tag: k.tag, Level: k.level}
}

// Bytes estimates the live size of the dictionary and the per-block class
// lists of every indexed segment (issue #37 mem.index_kib).
func (s *Store) IndexBytes() int64 {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	var n int64
	for _, k := range s.classes.keys {
		n += int64(48 + len(k.pkg) + len(k.proc) + len(k.tag))
	}
	n += int64(len(s.classes.of) * 40)
	for _, ix := range s.idx {
		for _, b := range ix.blocks {
			n += int64(len(b.classes) * 8)
			if b.uncounted {
				n += 8
			}
		}
	}
	return n
}

// EnsureIndex builds (or extends) the block index of every segment so class
// counts and minT/maxT are up to date. Safe to call repeatedly; cheap when
// nothing grew. Issue #37.
func (s *Store) EnsureIndex() {
	s.mu.RLock()
	segs := append([]segment(nil), s.segs...)
	s.mu.RUnlock()
	for _, sg := range segs {
		s.segIndexFor(sg.path)
	}
}

// MatchingClassIDs evaluates match on every dictionary class and returns the
// set of matching ids. Call EnsureIndex first so the dictionary covers every
// stored record. match must depend only on class fields.
func (s *Store) MatchingClassIDs(match func(*model.Record) bool) map[uint32]bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	out := map[uint32]bool{}
	for id, k := range s.classes.keys {
		if match(k.Record()) {
			out[uint32(id)] = true
		}
	}
	return out
}

// classSum returns the sum of counts for ids present in b.classes.
// ok is false when the block is uncounted and must be decoded.
func (b block) classSum(ids map[uint32]bool) (n uint64, ok bool) {
	if b.uncounted {
		return 0, false
	}
	for _, c := range b.classes {
		if ids[c.id] {
			n += uint64(c.n)
		}
	}
	return n, true
}

// classMatch reports whether b may contain a matching record: true when the
// block is uncounted, or when any of its classes is in ids. ids==nil means
// "every class matches" (no class filter).
func (b block) classMatch(ids map[uint32]bool) bool {
	if ids == nil {
		return true
	}
	if b.uncounted {
		return true // must decode
	}
	for _, c := range b.classes {
		if ids[c.id] {
			return true
		}
	}
	return false
}

// whollyInside reports whether every record of b lies in [fromNs, toNs)
// (0 = unbounded side). A block with an unreadable time is never wholly inside.
func (b block) whollyInside(fromNs, toNs int64) bool {
	if b.minT == mathMinInt64 || b.maxT == mathMaxInt64 {
		return false
	}
	if fromNs != 0 && b.minT < fromNs {
		return false
	}
	if toNs != 0 && b.maxT >= toNs {
		return false
	}
	return true
}

// math extremes aliased so class.go does not need to import math for the
// two sentinels history.go already uses.
const (
	mathMinInt64 = int64(-1 << 63)
	mathMaxInt64 = int64(1<<63 - 1)
)

// lineClass extracts the class fields of one stored line without a full
// decode. Missing uid is -1; missing level is 0; missing strings are empty.
// ok is false when the line is not a compact record the store writes.
func lineClass(line []byte) (k classKey, ok bool) {
	k.uid = -1
	if !bytes.HasPrefix(line, []byte(`{"s":`)) {
		return k, false
	}
	// Walk known single-letter keys the same way decodeFast does, but skip
	// string bodies we do not need (boot, message, buffer) without allocating.
	n := len(line)
	for n > 0 && (line[n-1] == '\n' || line[n-1] == '\r' || line[n-1] == ' ') {
		n--
	}
	b := line[:n]
	if len(b) < 2 || b[0] != '{' || b[len(b)-1] != '}' {
		return k, false
	}
	i := 1
	for i < len(b) {
		if i+4 > len(b) || b[i] != '"' || b[i+2] != '"' || b[i+3] != ':' {
			return k, false
		}
		key := b[i+1]
		i += 4
		var good bool
		switch key {
		case 'u':
			k.uid, i, good = parseIntField(b, i)
		case 'l':
			var v uint64
			v, i, good = parseUint(b, i)
			good = good && v <= 255
			k.level = model.Level(v)
		case 'g':
			k.tag, i, good = parseString(b, i)
		case 'n':
			k.proc, i, good = parseString(b, i)
		case 'k':
			k.pkg, i, good = parseString(b, i)
		case 's', 't', 'p', 'i':
			_, i, good = parseInt(b, i)
		case 'b', 'm', 'f':
			_, i, good = skipString(b, i)
		default:
			return k, false
		}
		if !good || i >= len(b) {
			return k, false
		}
		switch b[i] {
		case ',':
			i++
		case '}':
			return k, i == len(b)-1
		default:
			return k, false
		}
	}
	return k, false
}

// skipString advances past a JSON string starting at b[i], returning the
// empty string (the value is discarded).
func skipString(b []byte, i int) (string, int, bool) {
	_, i, ok := parseString(b, i)
	return "", i, ok
}

// sealClasses converts a working id→count map into a sorted class list on b.
// More than MaxBlockClasses distinct ids marks the block uncounted.
func sealClasses(b *block, counts map[uint32]uint32) {
	if counts == nil {
		b.uncounted = true
		b.classes = nil
		return
	}
	if len(counts) > MaxBlockClasses {
		b.uncounted = true
		b.classes = nil
		return
	}
	b.uncounted = false
	cs := make([]classCount, 0, len(counts))
	for id, n := range counts {
		cs = append(cs, classCount{id: id, n: n})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].id < cs[j].id })
	b.classes = cs
}

// openCounts rebuilds a working map from a block's sealed class list, or
// nil when the block is uncounted (further lines keep it uncounted).
func openCounts(b block) map[uint32]uint32 {
	if b.uncounted {
		return nil
	}
	m := map[uint32]uint32{}
	for _, c := range b.classes {
		m[c.id] = c.n
	}
	return m
}

// addClass increments the working map for id; past the cap the map becomes
// nil (uncounted).
func addClass(counts map[uint32]uint32, id uint32) map[uint32]uint32 {
	if counts == nil {
		return nil
	}
	if _, ok := counts[id]; !ok && len(counts) >= MaxBlockClasses {
		return nil
	}
	counts[id]++
	return counts
}

// classProcLabel mirrors mcpserver.procLabel for summary process counts.
func classProcLabel(r *model.Record) string {
	label := r.Process
	if label == "" {
		switch {
		case r.Package != "":
			label = "uid:" + r.Package
		case r.UID >= 0:
			label = "uid:" + model.UIDName(r.UID)
		default:
			return "?"
		}
	}
	if strings.ContainsAny(label, " \t") {
		label = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' {
				return '_'
			}
			return r
		}, label)
	}
	return label
}

// AggClasses returns the exact total and per-level / per-tag / per-process
// counts for a countable filter over (after, before) ∩ [fromNs, toNs).
// Same block rules as CountMatch (issue #37).
func (s *Store) AggClasses(after, before uint64, fromNs, toNs int64, ids map[uint32]bool, match func(*model.Record) bool) (total int, levels map[int]int, tags, procs map[string]int) {
	levels = map[int]int{}
	tags = map[string]int{}
	procs = map[string]int{}
	add := func(r *model.Record, n int) {
		total += n
		levels[r.Level.Rank()] += n
		tags[r.Tag] += n
		procs[classProcLabel(r)] += n
	}
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
	if after+1 >= before {
		return
	}
	lo, hi := after+1, before
	inTime := func(r *model.Record) bool {
		return (fromNs == 0 || r.TimeNs >= fromNs) && (toNs == 0 || r.TimeNs < toNs)
	}
	memFirst := uint64(0)
	if len(mem) > 0 {
		memFirst = mem[0].Seq
		if mlo, mhi := max(lo, memFirst), min(hi, memFirst+uint64(len(mem))); mhi > mlo {
			for q := mlo; q < mhi; q++ {
				r := mem[q-memFirst]
				if inTime(r) && match(r) {
					add(r, 1)
				}
			}
		}
		hi = min(hi, memFirst)
	}
	if lo >= hi {
		return
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
			whollySeq := b.first >= lo && b.last < hi
			if whollySeq && b.whollyInside(fromNs, toNs) && !b.uncounted {
				type cn struct {
					k classKey
					n int
				}
				var addl []cn
				s.hmu.Lock()
				for _, c := range b.classes {
					if ids[c.id] {
						addl = append(addl, cn{s.classes.Key(c.id), int(c.n)})
					}
				}
				s.hmu.Unlock()
				for _, x := range addl {
					add(x.k.Record(), x.n)
				}
				continue
			}
			for _, r := range s.blockRecords(sg.path, b) {
				if r.Seq >= lo && r.Seq < hi && inTime(r) && match(r) {
					add(r, 1)
				}
			}
		}
	}
	return
}
