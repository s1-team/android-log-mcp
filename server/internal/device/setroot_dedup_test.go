package device

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #56: after a set-root move the new root has no stored lines, so
// resumes must compare with the lines the old root stored (carried in
// resume_dedup.bin), on the first resume and on the ones after it.

func managerAt(t *testing.T, root string) (*Manager, func()) {
	t.Helper()
	m := newTestManager(t, root, store.Config{})
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	m.Linger = func(*Device) time.Duration { return time.Minute }
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	var once sync.Once
	closeFn := func() { once.Do(func() { cancel(); m.Close() }) }
	t.Cleanup(closeFn)
	return m, closeFn
}

func countIn(st *store.Store, prefix string) int {
	n := 0
	for _, m := range messages(st) {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

// The device clock is set back after the move (whole-buffer read, with
// -T unusable): no line of the old root is stored again, on the first and
// on the second resume in the new root, and no new line is lost.
func TestSetRootThenWholeReadNoDuplicates(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 0, "FAKEA")
	now := time.Now()
	var old []string
	for i := 0; i < 6; i++ { // minutes apart: far outside the 10 s boundary window
		old = append(old, fakeadb.Line(now.Add(-time.Duration(40-i*5)*time.Minute), 100, "OLD", fmt.Sprintf("old %d", i)))
	}
	at := now.Add(-3 * time.Minute) // the same line twice in the same µs (a multiset)
	dup := fakeadb.Line(at, 100, "DUP", "old same line")
	other := fakeadb.Line(at, 101, "OTHER", "old between")
	old = append(old, dup, other, dup)
	e.Log("FAKEA", old...)

	oldRoot, newRoot := t.TempDir(), filepath.Join(t.TempDir(), "new")
	m1, close1 := managerAt(t, oldRoot)
	d := startDevice(t, m1, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == len(old) })
	reconnect(t, e, m1, "FAKEA")
	close1()
	cs, err := CarryOver(oldRoot, newRoot)
	if err != nil || len(cs) != 1 {
		t.Fatalf("carry: %+v %v", cs, err)
	}

	back := now.Add(-24 * time.Hour) // the device clock is set back after the move
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back, 100, "NEW", "new 1"), fakeadb.Line(back.Add(time.Millisecond), 100, "NEW", "new 2"))
	m2, _ := managerAt(t, newRoot)
	d, stream := resume(t, e, m2, "FAKEA")
	if strings.Contains(stream, " -T ") {
		t.Fatalf("expected a whole-buffer read: %s", stream)
	}
	// the pipeline stores in order: with the last new line stored the
	// whole-buffer read before it was processed
	waitFor(t, "new 2", 30*time.Second, func() bool { return countMsg(d.Store, "new 2") == 1 })
	if n, o := countIn(d.Store, "new "), countIn(d.Store, "old "); n != 2 || o != 0 || len(messages(d.Store)) != 2 {
		t.Fatalf("first resume stored %d new, %d old lines: %q", n, o, messages(d.Store))
	}
	if _, err := os.Stat(filepath.Join(d.Store.Dir, store.ResumeDedupFile)); err != nil {
		t.Fatalf("carried set dropped after the first resume: %v", err)
	}

	// a second whole-buffer read in the new root (the old code applied
	// the carried set to the first one only)
	reconnect(t, e, m2, "FAKEA")
	e.Log("FAKEA", fakeadb.Line(back.Add(2*time.Millisecond), 100, "NEW", "new 3"))
	d, stream = resume(t, e, m2, "FAKEA")
	if strings.Contains(stream, " -T ") {
		t.Fatalf("expected a whole-buffer read: %s", stream)
	}
	waitFor(t, "new 3", 30*time.Second, func() bool { return countMsg(d.Store, "new 3") == 1 })
	if n, o := countIn(d.Store, "new "), countIn(d.Store, "old "); n != 3 || o != 0 || len(messages(d.Store)) != 3 {
		t.Fatalf("second resume stored %d new, %d old lines: %q", n, o, messages(d.Store))
	}
	// the old root's lines are still in the device buffer exactly once each
	// in the old root: nothing was lost or duplicated across both roots
	ost, err := store.OpenReadOnly(cs[0].From, store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ost.Close()
	if got, want := len(messages(ost))+len(messages(d.Store)), len(bufferKeys(t, e, "FAKEA")); got != want {
		t.Fatalf("stored %d lines across both roots, the buffer holds %d", got, want)
	}
	if fi, err := os.Stat(filepath.Join(d.Store.Dir, "meta.json")); err != nil || fi.Size() > 16<<10 {
		t.Fatalf("meta.json is %v bytes (%v): hashes belong in %s", fi.Size(), err, store.ResumeDedupFile)
	}
}

// A carried set made by 0.5.x–0.6.0 (inline hashes in meta.json) is still
// read, and a move with nothing to carry writes no file.
func TestLoadResumeDedupLegacy(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpdateMeta(func(m *store.Meta) { m.ResumeDedup = &store.ResumeDedup{TimeNs: 5, Hashes: []uint64{1, 2, 2}} })
	if h := st.LoadResumeDedup(); len(h) != 3 {
		t.Fatalf("legacy hashes: %v", h)
	}
	if !st.Meta().ResumeDedup.Active("b", 5, 100) || st.Meta().ResumeDedup.Active("b", 6, 100) {
		t.Fatal("legacy set applies only while the newest arrival is its resume point")
	}
	st.ClearResumeDedup()
	if st.Meta().ResumeDedup != nil || st.LoadResumeDedup() != nil {
		t.Fatal("not cleared")
	}
}
