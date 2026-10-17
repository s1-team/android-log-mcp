package device

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #191: a failed Store.Append keeps the batch queued, and Appended()
// counts only stored records.
func TestDrainKeepsBatchWhenAppendFails(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(store.DataDir(t.TempDir()), "x")
	w, err := store.OpenDir(dir, "x", store.Config{IdleRelease: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenDir(dir, "x", store.Config{IdleRelease: -1}, true) // follower: Append fails
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := NewPipeline(f, "b1", time.UTC)
	now := time.Now()
	for i := 0; i < 4; i++ {
		p.Accept(&model.Record{TimeNs: now.UnixNano() + int64(i)*1e6, PID: 10, TID: 10, Level: 'I', Tag: "T", Msg: "m"}, now)
	}
	if err := p.Drain(true, now); err == nil {
		t.Fatal("Drain succeeded on a store that cannot append")
	}
	if p.Pending() != 4 || p.Appended() != 0 {
		t.Fatalf("after a failed Drain: pending=%d appended=%d, want 4 and 0", p.Pending(), p.Appended())
	}
	w.Close()
	if !f.TryPromote() {
		t.Fatal("promote")
	}
	if err := p.Drain(true, now); err != nil {
		t.Fatal(err)
	}
	if p.Pending() != 0 || p.Appended() != 4 {
		t.Fatalf("after the retry: pending=%d appended=%d, want 0 and 4", p.Pending(), p.Appended())
	}
	if n := len(messages(f)); n != 4 {
		t.Fatalf("%d records stored, want 4", n)
	}
}
