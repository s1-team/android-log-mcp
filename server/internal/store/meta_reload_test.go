package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Issue #183: a follower re-reads meta.json whenever it changed, also when
// the file's mtime is older than the one it read (host clock set back).
func TestFollowerReloadsMetaAfterClockSetBack(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	w, err := OpenDir(dir, "x", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.UpdateMeta(func(m *Meta) { m.Model = "one" })
	f, err := OpenDir(dir, "x", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Refresh()
	if got := f.Meta().Model; got != "one" {
		t.Fatalf("follower sees model %q, want one", got)
	}
	w.UpdateMeta(func(m *Meta) { m.Model = "two" }) // same size as "one"
	past := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "meta.json"), past, past) // stamped before what the follower read
	f.Refresh()
	if got := f.Meta().Model; got != "two" {
		t.Fatalf("follower stuck on stale meta: model %q, want two", got)
	}
	// unchanged file: no reload needed, same answer
	f.Refresh()
	if got := f.Meta().Model; got != "two" {
		t.Fatalf("model %q after an idle refresh", got)
	}
}

// Issue #187: a failed save of meta.json stays dirty and Flush retries it.
func TestFailedMetaSaveIsRetriedByFlush(t *testing.T) {
	dir := filepath.Join(DataDir(t.TempDir()), "x")
	w, err := OpenDir(dir, "x", Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Flush()
	os.Remove(filepath.Join(dir, "meta.json"))
	// a directory where the temporary file goes: the save cannot be written
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	w.UpdateMeta(func(m *Meta) { m.Model = "kept" })
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		t.Fatal("meta.json written although the save was blocked")
	}
	w.Flush() // still blocked: stays dirty
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		t.Fatal("meta.json written although the save was blocked")
	}
	os.Remove(tmp)
	w.Flush()
	ro, err := OpenReadOnly(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got := ro.Meta().Model; got != "kept" {
		t.Fatalf("after the retry meta.json has model %q, want kept", got)
	}
}
