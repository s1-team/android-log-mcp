package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #75: one device buffer (system) can keep its boot lines while
// another (main) rotated many times, so the stored lines a whole buffer
// read can hold are not bounded by the buffer's entries.

func countMsg(st *store.Store, msg string) int {
	n := 0
	for _, m := range messages(st) {
		if m == msg {
			n++
		}
	}
	return n
}

// A whole-buffer resume after a clock jump compares with every stored line
// of the session, not the newest 1.5 × the buffer's entries.
func TestWholeResumeKeepsOldestBufferLines(t *testing.T) {
	t.Parallel()
	e := fakeEnv(t)
	addDevices(t, e, 0, "FAKEA")
	now := time.Now()
	var lines []string
	for i := 0; i < 3; i++ {
		lines = append(lines, fakeadb.Line(now.Add(-time.Hour+time.Duration(i)*time.Millisecond), 50, "BOOT", fmt.Sprintf("boot %d", i)))
	}
	for i := 0; i < 40; i++ {
		lines = append(lines, fakeadb.Line(now.Add(-time.Minute+time.Duration(i)*time.Millisecond), 100, "MAIN", fmt.Sprintf("main %d", i)))
	}
	e.Log("FAKEA", lines...)
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 43 })
	back := now.Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back, 100, "LIVE", "set back"))
	waitFor(t, "live line", 5*time.Second, func() bool { return len(messages(d.Store)) == 44 })
	reconnect(t, e, m, "FAKEA")
	e.DropLines("FAKEA", "MAIN") // main rotated: 4 entries left, 3 of them the oldest stored lines
	d, _ = resume(t, e, m, "FAKEA")
	time.Sleep(3 * time.Second) // past the pipeline delay
	for i := 0; i < 3; i++ {
		if n := countMsg(d.Store, fmt.Sprintf("boot %d", i)); n != 1 {
			t.Fatalf("boot %d stored %d times", i, n)
		}
	}
	if n := len(messages(d.Store)); n != 44 {
		t.Fatalf("stored %d lines, want 44", n)
	}
}

// A repair read compares with every stored line of the session: the
// dump's first records stored recently and the next ones stored long ago
// (1000s of lines before) are all stored; only the real hole is returned.
func TestHolesWholeSession(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var old, filler, recent []*model.Record
	for i := 0; i < 5; i++ {
		old = append(old, hrec(int64(1+i), 2, "BOOT", fmt.Sprint("boot ", i)))
	}
	for i := 0; i < 3000; i++ {
		filler = append(filler, &model.Record{TimeNs: 100e9 + int64(i)*1e6, PID: 3, TID: 3, Level: 'I', Tag: "ROT", Msg: fmt.Sprint("rotated ", i)})
	}
	for i := 0; i < 22; i++ {
		recent = append(recent, hrec(int64(500+i), 4, "NEW", fmt.Sprint("new ", i)))
	}
	st.Append(old)
	st.Append(filler)
	st.Append(recent[:21])
	st.Flush()
	dump := append(append([]*model.Record{}, recent[:20]...), old...)
	dump = append(dump, hrec(530, 4, "HOLE", "skipped"), recent[20], recent[21])
	var x sessionIndex
	x.update(st)
	got := x.holes(dump)
	if len(got) != 1 || got[0].Tag != "HOLE" {
		t.Fatalf("holes %d, want only HOLE: %v", len(got), got)
	}
	// the index follows the store: recent[21] stored now
	st.Append([]*model.Record{recent[21]})
	st.Flush()
	x.update(st)
	if x.recs[recHash(recent[21])] != 1 || x.recs[recHash(old[0])] != 1 {
		t.Fatalf("index not extended")
	}
}
