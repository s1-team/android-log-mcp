package store

import (
	"encoding/binary"
	"hash/crc64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// fillPastRotate appends until at least one sealed (non-last) segment exists.
func fillPastRotate(t *testing.T, s *Store) {
	t.Helper()
	for i := 0; len(ListSegments(s.Dir)) < 2; i++ {
		r := &model.Record{
			Seq: s.LastSeq() + 1, Boot: "boot", TimeNs: int64(i + 1),
			UID: 1000, PID: 1, TID: 1, Level: model.Info, Tag: "T",
			Msg:    "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			Buffer: "main", Process: "p", Package: "pkg",
		}
		if err := s.Append([]*model.Record{r}); err != nil {
			t.Fatal(err)
		}
		if i > 100000 {
			t.Fatal("failed to rotate")
		}
	}
}

func TestIdxSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil || !s.IsWriter() {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	segs := ListSegments(s.Dir)
	if len(segs) < 2 {
		t.Fatal("need closed segment")
	}
	closed := segs[0]
	idxP := idxPathBeside(closed.Path)
	if _, err := os.Stat(idxP); err != nil {
		t.Fatalf("expected sidecar after EnsureIndex: %v", err)
	}
	s.DropIndexes()
	s.EnsureIndex()
	ix1 := s.segIndexFor(closed.Path, true)
	if ix1 == nil || ix1.end == 0 || len(ix1.blocks) == 0 {
		t.Fatalf("load failed: %+v", ix1)
	}
	s.DropIndexes()
	s.RemoveIndexSidecars()
	s.EnsureIndex()
	ix2 := s.segIndexFor(closed.Path, true)
	if ix2 == nil || len(ix2.blocks) != len(ix1.blocks) || ix2.end != ix1.end {
		t.Fatalf("rebuild mismatch load blocks=%d end=%d rebuild blocks=%d end=%d",
			len(ix1.blocks), ix1.end, len(ix2.blocks), ix2.end)
	}
	for i := range ix1.blocks {
		a, b := ix1.blocks[i], ix2.blocks[i]
		if a.off != b.off || a.end != b.end || a.first != b.first || a.last != b.last || a.n != b.n || a.minT != b.minT || a.maxT != b.maxT || a.uncounted != b.uncounted {
			t.Fatalf("block %d meta: %+v vs %+v", i, a, b)
		}
		if len(a.classes) != len(b.classes) {
			t.Fatalf("block %d classes len %d vs %d", i, len(a.classes), len(b.classes))
		}
	}
}

func TestIdxMissCorruptByte(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	closed := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(closed.Path)
	raw, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(idxP, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.DropIndexes()
	s.EnsureIndex()
	raw2, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	if crc64.Checksum(raw, idxCRCTable) == crc64.Checksum(raw2, idxCRCTable) {
		t.Fatal("corrupt idx was not rewritten")
	}
	ix := s.segIndexFor(closed.Path, true)
	if ix == nil || len(ix.blocks) == 0 {
		t.Fatal("expected rebuilt index")
	}
}

func TestIdxMissSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	closed := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(closed.Path)
	raw, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(raw[16:24], uint64(1))
	if err := os.WriteFile(idxP, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.DropIndexes()
	s.EnsureIndex()
	raw2, _ := os.ReadFile(idxP)
	got := int64(binary.LittleEndian.Uint64(raw2[16:24]))
	st, _ := os.Stat(closed.Path)
	if got != st.Size() {
		t.Fatalf("rewritten seg_size=%d want %d", got, st.Size())
	}
}

func TestIdxMissBadSegCRC(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	closed := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(closed.Path)
	raw, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(raw[24:32], 0xdeadbeef)
	if err := os.WriteFile(idxP, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.DropIndexes()
	s.EnsureIndex()
	raw2, _ := os.ReadFile(idxP)
	if binary.LittleEndian.Uint64(raw2[24:32]) == 0xdeadbeef {
		t.Fatal("bad seg_crc64 idx not rewritten")
	}
}

func TestIdxOpenSegmentNeverLoads(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	segs := ListSegments(s.Dir)
	open := segs[len(segs)-1]
	closed := segs[0]
	// Plant a stale sidecar for the open segment; EnsureIndex must ignore it.
	stale := make([]byte, idxHeaderSize+8)
	copy(stale, idxMagic)
	binary.LittleEndian.PutUint32(stale[4:], idxFormatVersion)
	first, _ := firstFromSegPath(open.Path)
	binary.LittleEndian.PutUint64(stale[8:], first)
	st, _ := os.Stat(open.Path)
	binary.LittleEndian.PutUint64(stale[16:], uint64(st.Size()))
	if err := os.WriteFile(idxPathBeside(open.Path), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	s.DropIndexes()
	s.EnsureIndex()
	_ = os.Remove(idxPathBeside(open.Path))
	s.DropIndexes()
	s.EnsureIndex()
	if _, err := os.Stat(idxPathBeside(open.Path)); err == nil {
		t.Fatal("open segment must not get a sidecar")
	}
	if _, err := os.Stat(idxPathBeside(closed.Path)); err != nil {
		t.Fatal("closed segment should still have sidecar")
	}
	_ = time.Now() // keep import if unused otherwise
}

func TestIdxPruneDeletesSidecar(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 2 << 10, MemRecords: 5, BlockBytes: 512, MaxBytes: 6 << 10, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; len(ListSegments(s.Dir)) < 3; i++ {
		r := &model.Record{
			Seq: s.LastSeq() + 1, Boot: "b", TimeNs: int64(i + 1),
			UID: 1, PID: 1, TID: 1, Level: model.Info, Tag: "T",
			Msg:    "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			Buffer: "main", Process: "p", Package: "pkg",
		}
		if err := s.Append([]*model.Record{r}); err != nil {
			t.Fatal(err)
		}
		if i > 50000 {
			t.Fatal("no rotate")
		}
	}
	s.EnsureIndex()
	before := ListSegments(s.Dir)
	var goneCandidates []string
	for _, sg := range before[:len(before)-1] {
		p := idxPathBeside(sg.Path)
		if _, err := os.Stat(p); err == nil {
			goneCandidates = append(goneCandidates, p)
		}
	}
	if len(goneCandidates) == 0 {
		t.Fatal("no closed idx before prune")
	}
	s.mu.Lock()
	s.cfg.MaxBytes = 1
	s.pruneLocked()
	s.mu.Unlock()
	for _, p := range goneCandidates {
		if _, err := os.Stat(p); err == nil {
			jsonl := filepath.Join(dir, strings.TrimSuffix(filepath.Base(p), ".idx")+".jsonl")
			if _, jerr := os.Stat(jsonl); os.IsNotExist(jerr) {
				t.Fatalf("orphan idx left behind: %s", p)
			}
		}
	}
}

func TestIdxRemoveOldestSegmentFreesIdxBytes(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 2 << 10, MemRecords: 5, BlockBytes: 512, IdleRelease: -1}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; len(ListSegments(s.Dir)) < 2; i++ {
		r := &model.Record{
			Seq: s.LastSeq() + 1, Boot: "b", TimeNs: int64(i + 1),
			UID: 1, PID: 1, TID: 1, Level: model.Info, Tag: "T",
			Msg:    "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			Buffer: "main", Process: "p", Package: "pkg",
		}
		if err := s.Append([]*model.Record{r}); err != nil {
			t.Fatal(err)
		}
		if i > 50000 {
			t.Fatal("no rotate")
		}
	}
	s.EnsureIndex()
	old := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(old.Path)
	ifi, err := os.Stat(idxP)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := s.RemoveOldestSegment(old.Path)
	if !ok {
		t.Fatal("RemoveOldestSegment failed")
	}
	if n < old.Size+ifi.Size() {
		t.Fatalf("freed %d want at least jsonl+idx %d+%d", n, old.Size, ifi.Size())
	}
	if _, err := os.Stat(idxP); !os.IsNotExist(err) {
		t.Fatal("idx should be gone")
	}
}

func TestIdxSkipSegCRC64AcceptsBadSegCRC(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1, SkipSegCRC64: true}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	closed := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(closed.Path)
	raw, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(raw[24:32], 0xdeadbeef)
	if err := os.WriteFile(idxP, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	before := crc64.Checksum(raw, idxCRCTable)
	s.DropIndexes()
	s.EnsureIndex()
	raw2, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	if crc64.Checksum(raw2, idxCRCTable) != before {
		t.Fatal("skip path should keep idx with bad seg_crc64 (not rebuild)")
	}
	if binary.LittleEndian.Uint64(raw2[24:32]) != 0xdeadbeef {
		t.Fatal("seg_crc64 field should still be planted value under skip")
	}
	ix := s.segIndexFor(closed.Path, true)
	if ix == nil || len(ix.blocks) == 0 {
		t.Fatal("expected loaded index under skip with bad seg_crc64")
	}
}

func TestIdxSkipSegCRC64StillFailsBadPayloadCRC(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentBytes: 4 << 10, MemRecords: 10, BlockBytes: 1 << 10, IdleRelease: -1, SkipSegCRC64: true}
	s, err := OpenDir(dir, "d", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillPastRotate(t, s)
	s.EnsureIndex()
	closed := ListSegments(s.Dir)[0]
	idxP := idxPathBeside(closed.Path)
	raw, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt payload while leaving header (incl. stale payload_crc64) intact.
	if len(raw) <= idxHeaderSize {
		t.Fatal("idx too short")
	}
	raw[idxHeaderSize] ^= 0xff
	if err := os.WriteFile(idxP, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	before := crc64.Checksum(raw, idxCRCTable)
	s.DropIndexes()
	s.EnsureIndex()
	raw2, err := os.ReadFile(idxP)
	if err != nil {
		t.Fatal(err)
	}
	if crc64.Checksum(raw2, idxCRCTable) == before {
		t.Fatal("corrupt payload must fail closed and rewrite idx even with skip on")
	}
	ix := s.segIndexFor(closed.Path, true)
	if ix == nil || len(ix.blocks) == 0 {
		t.Fatal("expected rebuilt index after bad payload")
	}
}

func TestIdxSkipSegCRC64Env(t *testing.T) {
	t.Setenv(SkipSegCRC64Env, "1")
	c := ConfigFromEnv()
	if !c.SkipSegCRC64 {
		t.Fatal("expected SkipSegCRC64 from env=1")
	}
	t.Setenv(SkipSegCRC64Env, "true")
	if !ConfigFromEnv().SkipSegCRC64 {
		t.Fatal("expected SkipSegCRC64 from env=true")
	}
	t.Setenv(SkipSegCRC64Env, "")
	if ConfigFromEnv().SkipSegCRC64 {
		t.Fatal("empty env must keep default verify")
	}
	t.Setenv(SkipSegCRC64Env, "0")
	if ConfigFromEnv().SkipSegCRC64 {
		t.Fatal("env=0 must keep default verify")
	}
	if !EnvOn("yes") || !EnvOn("ON") || EnvOn("no") || EnvOn("") {
		t.Fatal("EnvOn truth table")
	}
}
