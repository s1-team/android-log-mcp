package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Issue #196: RewriteAux, like AppendAux and CompactAux, is for the writer.
func TestRewriteAuxRequiresWriter(t *testing.T) {
	t.Parallel()
	w, err := Open(t.TempDir(), "d", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendAux("crashes", map[string]string{"id": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.RewriteAux("crashes", [][]byte{[]byte(`{"id":"b"}`)}); err != nil {
		t.Fatalf("the writer: %v", err)
	}
	w.Close()
	ro, err := OpenReadOnly(w.Dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.RewriteAux("crashes", nil); err == nil {
		t.Fatalf("a read-only store rewrote crashes.jsonl (now %d lines)", len(ro.ReadAux("crashes")))
	}
	if got := ro.ReadAux("crashes"); len(got) != 1 || string(got[0]) != `{"id":"b"}` {
		t.Fatalf("file changed: %q", got)
	}
}

// A failed rewrite (here: the target is a directory, so the rename fails)
// removes its .jsonl.tmp; the same through CompactAux.
func TestRewriteAuxFailureRemovesTmp(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), "d", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.MkdirAll(filepath.Join(s.Dir, "x.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.RewriteAux("x", [][]byte{[]byte(`{"a":1}`)}); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "x.jsonl.tmp")); err == nil {
		t.Fatal("a failed RewriteAux left x.jsonl.tmp")
	}
	// the temp file cannot even be written: nothing is left either
	if err := os.MkdirAll(filepath.Join(s.Dir, "y.jsonl.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.RewriteAux("y", [][]byte{[]byte(`{"a":1}`)}); err == nil {
		t.Fatal("expected a failure")
	}
}
