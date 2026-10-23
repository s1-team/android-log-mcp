package layout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func TestDeviceFileRecognizesIdx(t *testing.T) {
	if !deviceFile("seg-00000000000000000001.idx") {
		t.Fatal("seg-*.idx must be a deviceFile")
	}
	if !deviceFile("seg-00000000000000000001.idx.tmp") {
		t.Fatal("seg-*.idx.tmp must be a deviceFile")
	}
	if deviceFile("other.idx") {
		t.Fatal("non-seg .idx must not be a deviceFile")
	}
}

func TestSweepDeletesIdxWithSegment(t *testing.T) {
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 2 << 10, MemRecords: 5, BlockBytes: 512, IdleRelease: -1, MaxAge: time.Hour}
	s, err := store.Open(base, "dev", cfg)
	if err != nil || !s.IsWriter() {
		t.Fatalf("open: %v", err)
	}
	for i := 0; len(store.ListSegments(s.Dir)) < 2; i++ {
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
	closed := store.ListSegments(s.Dir)[0]
	if _, err := os.Stat(idxPathBesideLayout(closed.Path)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	s.Close() // release writer so Sweep can lock

	now := time.Now().Add(2 * time.Hour)
	res := Sweep(SweepOptions{Base: base, MaxAge: time.Hour, Now: now})
	if _, err := os.Stat(closed.Path); !os.IsNotExist(err) {
		t.Fatalf("segment should be expired: %v", err)
	}
	if _, err := os.Stat(idxPathBesideLayout(closed.Path)); !os.IsNotExist(err) {
		t.Fatal("idx should be deleted with segment")
	}
	if res.Files < 1 {
		t.Fatalf("expected deletions, Freed=%d Files=%d", res.Freed, res.Files)
	}
}

func TestSweepPurgesOrphanIdx(t *testing.T) {
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 4 << 20, MemRecords: 5, IdleRelease: -1}
	s, err := store.Open(base, "dev", cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &model.Record{
		Seq: 1, Boot: "b", TimeNs: 1, UID: 1, PID: 1, TID: 1,
		Level: model.Info, Tag: "T", Msg: "hi", Buffer: "main", Process: "p", Package: "pkg",
	}
	if err := s.Append([]*model.Record{r}); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	dir := s.Dir
	s.Close()
	orphan := filepath.Join(dir, "seg-00000000000000000099.idx")
	if err := os.WriteFile(orphan, []byte("ALMIx"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Sweep(SweepOptions{Base: base, Now: time.Now()})
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan idx should be purged")
	}
	if res.Files < 1 {
		t.Fatal("expected Files>=1")
	}
}

func idxPathBesideLayout(segPath string) string {
	if len(segPath) > 6 && segPath[len(segPath)-6:] == ".jsonl" {
		return segPath[:len(segPath)-6] + ".idx"
	}
	return segPath + ".idx"
}
