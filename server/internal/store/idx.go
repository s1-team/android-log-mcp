package store

import (
	"encoding/binary"
	"hash/crc64"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Segment index sidecar (issue #260 / #173 G1). Derived cache only: any
// validation miss rebuilds via extendIndex. Closed segments only; the open
// (last) segment stays memory-only. Format version 1.
//
// File layout (little-endian):
//
//	magic[4]="ALMI" | format_version u32=1 | first_seq u64 | seg_size i64 |
//	seg_crc64 u64 | payload_crc64 u64 | payload…
//
// Payload: per-segment class dictionary, then blocks (local class ids):
// off, first, last, n, minT, maxT, uncounted, classes.

const (
	idxMagic         = "ALMI"
	idxFormatVersion = uint32(1)
	idxHeaderSize    = 4 + 4 + 8 + 8 + 8 + 8 // 40
)

var idxCRCTable = crc64.MakeTable(crc64.ECMA)

func idxName(first uint64) string {
	return "seg-" + pad20(first) + ".idx"
}

func pad20(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) >= 20 {
		return s
	}
	return strings.Repeat("0", 20-len(s)) + s
}

// idxPathBeside returns the .idx path next to a segment .jsonl path.
func idxPathBeside(segPath string) string {
	if strings.HasSuffix(segPath, ".jsonl") {
		return strings.TrimSuffix(segPath, ".jsonl") + ".idx"
	}
	return segPath + ".idx"
}

func idxTmpBeside(segPath string) string {
	return idxPathBeside(segPath) + ".tmp"
}

// firstFromSegPath parses the first seq from a seg-%020d.jsonl path.
func firstFromSegPath(path string) (uint64, bool) {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "seg-") || !strings.HasSuffix(base, ".jsonl") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(base, "seg-"), ".jsonl"), 10, 64)
	return n, err == nil
}

// removeIdxBeside deletes the sidecar for segPath (best-effort).
// Returns the size removed (0 if absent or remove failed).
func removeIdxBeside(segPath string) int64 {
	p := idxPathBeside(segPath)
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if os.Remove(p) != nil {
		return 0
	}
	return fi.Size()
}

// crc64File streams path into an ECMA CRC64.
func crc64File(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc64.New(idxCRCTable)
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return h.Sum64(), nil
}

func putU16(buf *[]byte, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	*buf = append(*buf, b[:]...)
}

func putU32(buf *[]byte, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	*buf = append(*buf, b[:]...)
}

func putU64(buf *[]byte, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	*buf = append(*buf, b[:]...)
}

func putI64(buf *[]byte, v int64) { putU64(buf, uint64(v)) }

func putStr(buf *[]byte, s string) {
	if len(s) > 65535 {
		s = s[:65535]
	}
	putU16(buf, uint16(len(s)))
	*buf = append(*buf, s...)
}

// encodeIdxPayload builds the class-dict + blocks payload (caller holds hmu).
func (s *Store) encodeIdxPayload(ix *segIndex) []byte {
	globalToLocal := map[uint32]uint32{}
	var locals []classKey
	addID := func(gid uint32) uint32 {
		if lid, ok := globalToLocal[gid]; ok {
			return lid
		}
		lid := uint32(len(locals))
		globalToLocal[gid] = lid
		locals = append(locals, s.classes.Key(gid))
		return lid
	}
	type blkEnc struct {
		b     block
		local []classCount
		unc   bool
	}
	enc := make([]blkEnc, len(ix.blocks))
	for i, b := range ix.blocks {
		enc[i].b = b
		enc[i].unc = b.uncounted
		if b.uncounted {
			continue
		}
		cs := make([]classCount, len(b.classes))
		for j, c := range b.classes {
			cs[j] = classCount{id: addID(c.id), n: c.n}
		}
		enc[i].local = cs
	}

	buf := make([]byte, 0, 256+len(ix.blocks)*48)
	putU32(&buf, uint32(len(locals)))
	for _, k := range locals {
		putStr(&buf, k.pkg)
		putStr(&buf, k.proc)
		putStr(&buf, k.tag)
		putU32(&buf, uint32(int32(k.uid)))
		buf = append(buf, byte(k.level))
	}
	putU32(&buf, uint32(len(enc)))
	for _, e := range enc {
		putI64(&buf, e.b.off)
		putU64(&buf, e.b.first)
		putU64(&buf, e.b.last)
		putU32(&buf, uint32(e.b.n))
		putI64(&buf, e.b.minT)
		putI64(&buf, e.b.maxT)
		flags := byte(0)
		if e.unc {
			flags = 1
		}
		buf = append(buf, flags)
		if e.unc {
			continue
		}
		putU32(&buf, uint32(len(e.local)))
		for _, c := range e.local {
			putU32(&buf, c.id)
			putU32(&buf, c.n)
		}
	}
	return buf
}

// decodeIdxPayload merges the class dictionary into s.classes and returns
// the index (caller holds hmu). ok is false on any decode error.
func (s *Store) decodeIdxPayload(payload []byte, segSize int64) (*segIndex, bool) {
	getU32 := func(i int) (uint32, int, bool) {
		if i+4 > len(payload) {
			return 0, i, false
		}
		return binary.LittleEndian.Uint32(payload[i:]), i + 4, true
	}
	getU64 := func(i int) (uint64, int, bool) {
		if i+8 > len(payload) {
			return 0, i, false
		}
		return binary.LittleEndian.Uint64(payload[i:]), i + 8, true
	}
	getI64 := func(i int) (int64, int, bool) {
		u, i2, ok := getU64(i)
		return int64(u), i2, ok
	}
	getStr := func(i int) (string, int, bool) {
		if i+2 > len(payload) {
			return "", i, false
		}
		n := int(binary.LittleEndian.Uint16(payload[i:]))
		i += 2
		if n < 0 || i+n > len(payload) {
			return "", i, false
		}
		return string(payload[i : i+n]), i + n, true
	}

	i := 0
	nc, i, ok := getU32(i)
	if !ok {
		return nil, false
	}
	localToGlobal := make([]uint32, nc)
	for c := uint32(0); c < nc; c++ {
		var k classKey
		if k.pkg, i, ok = getStr(i); !ok {
			return nil, false
		}
		if k.proc, i, ok = getStr(i); !ok {
			return nil, false
		}
		if k.tag, i, ok = getStr(i); !ok {
			return nil, false
		}
		if i+4 > len(payload) {
			return nil, false
		}
		k.uid = int(int32(binary.LittleEndian.Uint32(payload[i:])))
		i += 4
		if i >= len(payload) {
			return nil, false
		}
		k.level = model.Level(payload[i])
		i++
		localToGlobal[c] = s.classes.id(k)
	}
	nb, i, ok := getU32(i)
	if !ok {
		return nil, false
	}
	blocks := make([]block, 0, nb)
	for b := uint32(0); b < nb; b++ {
		var blk block
		if blk.off, i, ok = getI64(i); !ok {
			return nil, false
		}
		if blk.first, i, ok = getU64(i); !ok {
			return nil, false
		}
		if blk.last, i, ok = getU64(i); !ok {
			return nil, false
		}
		var n32 uint32
		if n32, i, ok = getU32(i); !ok {
			return nil, false
		}
		blk.n = int(n32)
		if blk.minT, i, ok = getI64(i); !ok {
			return nil, false
		}
		if blk.maxT, i, ok = getI64(i); !ok {
			return nil, false
		}
		if i >= len(payload) {
			return nil, false
		}
		flags := payload[i]
		i++
		if flags&1 != 0 {
			blk.uncounted = true
		} else {
			var nc2 uint32
			if nc2, i, ok = getU32(i); !ok {
				return nil, false
			}
			cs := make([]classCount, nc2)
			for j := uint32(0); j < nc2; j++ {
				var lid, n uint32
				if lid, i, ok = getU32(i); !ok {
					return nil, false
				}
				if n, i, ok = getU32(i); !ok {
					return nil, false
				}
				if int(lid) >= len(localToGlobal) {
					return nil, false
				}
				cs[j] = classCount{id: localToGlobal[lid], n: n}
			}
			blk.classes = cs
		}
		blocks = append(blocks, blk)
	}
	if i != len(payload) {
		return nil, false
	}
	for j := range blocks {
		if j+1 < len(blocks) {
			blocks[j].end = blocks[j+1].off
		} else {
			blocks[j].end = segSize
		}
	}
	return &segIndex{blocks: blocks, end: segSize}, true
}

// buildIdxFile encodes a complete sidecar for a sealed segment (caller holds hmu).
func (s *Store) buildIdxFile(first uint64, segSize int64, segCRC uint64, ix *segIndex) []byte {
	payload := s.encodeIdxPayload(ix)
	pCRC := crc64.Checksum(payload, idxCRCTable)
	out := make([]byte, 0, idxHeaderSize+len(payload))
	out = append(out, idxMagic...)
	putU32(&out, idxFormatVersion)
	putU64(&out, first)
	putI64(&out, segSize)
	putU64(&out, segCRC)
	putU64(&out, pCRC)
	out = append(out, payload...)
	return out
}

// tryLoadIdx reads and validates the sidecar for a closed segment.
// On success installs into s.idx[path] and returns true (caller holds hmu).
// Opens the file briefly read-only and closes before CRC/decode work so a
// writer can rename on Windows.
func (s *Store) tryLoadIdx(path string, first uint64, segSize int64) bool {
	idxPath := idxPathBeside(path)
	raw, err := os.ReadFile(idxPath)
	if err != nil || len(raw) < idxHeaderSize {
		return false
	}
	if string(raw[:4]) != idxMagic {
		return false
	}
	ver := binary.LittleEndian.Uint32(raw[4:8])
	if ver != idxFormatVersion {
		return false
	}
	gotFirst := binary.LittleEndian.Uint64(raw[8:16])
	if gotFirst != first {
		return false
	}
	gotSize := int64(binary.LittleEndian.Uint64(raw[16:24]))
	if gotSize != segSize {
		return false
	}
	wantSegCRC := binary.LittleEndian.Uint64(raw[24:32])
	wantPayCRC := binary.LittleEndian.Uint64(raw[32:40])
	payload := raw[idxHeaderSize:]
	// Payload CRC always runs (fail-closed on corrupt idx payload).
	if crc64.Checksum(payload, idxCRCTable) != wantPayCRC {
		return false
	}
	// Full-segment CRC64 is the expensive cold-load check (~417 ms @ 512 MiB).
	// Opt-in skip via ANDROID_LOG_MCP_SKIP_SEG_CRC64 / Config.SkipSegCRC64 (#271);
	// size+first_seq still bind identity for append-only segments.
	if !s.cfg.SkipSegCRC64 {
		gotSegCRC, err := crc64File(path)
		if err != nil || gotSegCRC != wantSegCRC {
			return false
		}
	}
	ix, ok := s.decodeIdxPayload(payload, segSize)
	if !ok {
		return false
	}
	s.idx[path] = ix
	delete(s.idxPending, path)
	return true
}

// persistIdxLocked writes the sidecar for a closed, fully indexed segment
// (caller holds hmu). Writer only. tmp+WriteFile+Rename like saveMetaLocked;
// rename failure removes tmp and marks pending for retry.
func (s *Store) persistIdxLocked(path string, first uint64, segSize int64, ix *segIndex) {
	if !s.writer {
		return
	}
	if ix == nil || ix.end != segSize || segSize < 0 {
		return
	}
	segCRC, err := crc64File(path)
	if err != nil {
		return
	}
	raw := s.buildIdxFile(first, segSize, segCRC, ix)
	tmp := idxTmpBeside(path)
	dst := idxPathBeside(path)
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		_ = os.Remove(tmp)
		if s.idxPending == nil {
			s.idxPending = map[string]struct{}{}
		}
		s.idxPending[path] = struct{}{}
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		if s.idxPending == nil {
			s.idxPending = map[string]struct{}{}
		}
		s.idxPending[path] = struct{}{}
		return
	}
	delete(s.idxPending, path)
}

// flushIdxPendingLocked retries failed sidecar renames (caller holds hmu).
func (s *Store) flushIdxPendingLocked() {
	if !s.writer || len(s.idxPending) == 0 {
		return
	}
	pending := make([]string, 0, len(s.idxPending))
	for p := range s.idxPending {
		pending = append(pending, p)
	}
	for _, path := range pending {
		ix := s.idx[path]
		if ix == nil {
			delete(s.idxPending, path)
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			delete(s.idxPending, path)
			continue
		}
		first, ok := firstFromSegPath(path)
		if !ok || ix.end != st.Size() {
			continue
		}
		s.persistIdxLocked(path, first, st.Size(), ix)
	}
}

// RemoveIndexSidecars deletes every seg-*.idx in the device directory
// (tests / perf: force a cold rebuild). Best-effort.
func (s *Store) RemoveIndexSidecars() {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "seg-") && strings.HasSuffix(n, ".idx") && !strings.HasSuffix(n, ".idx.tmp") {
			_ = os.Remove(filepath.Join(s.Dir, n))
		}
	}
	s.hmu.Lock()
	s.idxPending = nil
	s.hmu.Unlock()
}
