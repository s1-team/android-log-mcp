package device

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #65: Android ≤ 11 logd makes a live reader skip entries while the
// device clock is set back; the lines stay in the device buffer and are
// recovered from a dump, once, without disturbing clock-jump detection.

func fastReconcile(t *testing.T) {
	testTiming(t).ReconcileEvery.Set(100 * time.Millisecond)
	testTiming(t).ReconcileSettle.Set(200 * time.Millisecond)
}

func backJumps(st *store.Store) int {
	n := 0
	for _, j := range st.ClockJumps() {
		if j.DeltaNs < 0 {
			n++
		}
	}
	return n
}

func TestSetBackSkippedLinesRecovered(t *testing.T) {
	t.Parallel()
	fastReconcile(t)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	if err := e.SetStreamSkip("FAKEA", "SKIPPED"); err != nil {
		t.Fatal(err)
	}
	back := time.Now().Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	for i := 0; i < 10; i++ {
		tag := "LIVE"
		if i >= 3 && i < 6 {
			tag = "SKIPPED"
		}
		e.Log("FAKEA", fakeadb.Line(back.Add(time.Duration(i)*time.Millisecond), 100+i, tag, fmt.Sprintf("after -1d %d", i)))
	}
	want := bufferKeys(t, e, "FAKEA")
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && fmt.Sprint(storedKeys(d.Store)) != fmt.Sprint(want); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // several more comparisons: nothing is added twice
	if got := storedKeys(d.Store); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
	if note := d.RecoveredNote(); !strings.Contains(note, "recovered 3 lines") || !strings.Contains(note, "repair reads on") {
		t.Fatalf("note %q", note)
	}
	if n := backJumps(d.Store); n != 1 {
		t.Fatalf("%d clock jumps back recorded, want 1: %+v", n, d.Store.ClockJumps())
	}
	// the clock back at real time: no stored line is dated after device
	// now any more, so the repair mode ends by itself
	e.SetDate("FAKEA", time.Time{})
	for i := 0; ; i++ {
		e.Log("FAKEA", fakeadb.Line(time.Now(), 200, "LIVE", fmt.Sprintf("back to now %d", i)))
		time.Sleep(200 * time.Millisecond)
		if !strings.Contains(d.RecoveredNote(), "repair reads on") {
			break
		}
		if i > 50 {
			t.Fatalf("the repair mode did not end: clock %+v", d.Clock())
		}
	}
	calls := len(reconcileCalls(e))
	time.Sleep(400 * time.Millisecond)
	if n := len(reconcileCalls(e)); n != calls {
		t.Fatalf("%d buffer dumps after the repair mode ended", n-calls)
	}
}

// A line logged twice (same device time, pid, tid, tag, text) of which the
// stream skipped one copy is recovered once: copies count one by one.
func TestSetBackSkippedIdenticalCopy(t *testing.T) {
	t.Parallel()
	fastReconcile(t)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	back := time.Now().Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	same := fakeadb.Line(back, 100, "SAME", "logged twice")
	// buffer lines 5..9; the stream skips the second copy (line 7)
	if err := e.SetStreamSkip("FAKEA", "#7"); err != nil {
		t.Fatal(err)
	}
	e.Log("FAKEA", same, fakeadb.Line(back.Add(time.Millisecond), 101, "MID", "between"), same,
		fakeadb.Line(back.Add(2*time.Millisecond), 102, "END", "after"), fakeadb.Line(back.Add(3*time.Millisecond), 103, "END", "last"))
	want := bufferKeys(t, e, "FAKEA")
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && fmt.Sprint(storedKeys(d.Store)) != fmt.Sprint(want); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if got := storedKeys(d.Store); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
	if note := d.RecoveredNote(); !strings.Contains(note, "recovered 1 lines") {
		t.Fatalf("note %q", note)
	}
}

// logd can kick the catch-up read too: lines missing at the very start
// of the device buffer are recovered when the session began there.
func TestSetBackSkippedCatchUpStart(t *testing.T) {
	t.Parallel()
	fastReconcile(t)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	if err := e.SetStreamSkip("FAKEA", "#0", "#1"); err != nil {
		t.Fatal(err)
	}
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 3 })
	back := time.Now().Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back, 100, "LIVE", "after -1d 0"), fakeadb.Line(back.Add(time.Millisecond), 100, "LIVE", "after -1d 1"))
	want := bufferKeys(t, e, "FAKEA")
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && fmt.Sprint(storedKeys(d.Store)) != fmt.Sprint(want); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if got := storedKeys(d.Store); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
}

// Without a clock set back nothing is compared (logd skips only then).
func TestNoReconcileWithoutSetBack(t *testing.T) {
	t.Parallel()
	fastReconcile(t)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	e.Log("FAKEA", fakeadb.Line(time.Now(), 100, "LIVE", "now"))
	waitFor(t, "live line", 5*time.Second, func() bool { return len(messages(d.Store)) == 6 })
	time.Sleep(400 * time.Millisecond)
	if c := reconcileCalls(e); len(c) != 0 {
		t.Fatalf("buffer dumped without a set-back: %q", c)
	}
}

// Android 12+ logd positions readers by sequence: no comparison.
func TestNoReconcileOnAPI31(t *testing.T) {
	t.Parallel()
	fastReconcile(t)
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	e.SetProp("FAKEA", "ro.build.version.sdk", "31")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	back := time.Now().Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	e.Log("FAKEA", fakeadb.Line(back, 100, "LIVE", "after -1d"))
	waitFor(t, "live line", 5*time.Second, func() bool { return len(messages(d.Store)) == 6 })
	time.Sleep(400 * time.Millisecond)
	if c := reconcileCalls(e); len(c) != 0 {
		t.Fatalf("buffer dumped on API 31: %q", c)
	}
}

func reconcileCalls(e fakeadb.Env) []string {
	var out []string
	for _, c := range e.CallsMatching("logcat -d -t") {
		if strings.HasSuffix(strings.TrimSpace(c), " -D") {
			out = append(out, c)
		}
	}
	return out
}

func hrec(sec int64, pid int, tag, msg string) *model.Record {
	return &model.Record{TimeNs: sec * 1e9, PID: pid, TID: pid, Level: 'I', Tag: tag, Msg: msg}
}

// holes: lines between stored ones and, when the compared lines reach the
// session's first line, lines before them; lines not streamed yet are never
// returned; a record stored split differently from the dump counts as stored.
func TestHoles(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Append([]*model.Record{hrec(10, 1, "A", "a"), hrec(12, 1, "C", "c part 1"), hrec(14, 1, "E", "e")})
	st.Flush()
	dump := []*model.Record{
		hrec(9, 1, "OLD", "skipped by the catch-up read"), // the session's first line is compared: lost too
		hrec(10, 1, "A", "a"),
		hrec(11, 1, "B", "b"),                  // skipped by the stream
		hrec(12, 1, "C", "c part 1\nc part 2"), // stored split: counts as stored
		hrec(13, 1, "D", "d"),                  // skipped
		hrec(14, 1, "E", "e"),
		hrec(15, 1, "F", "not streamed yet"),
	}
	var got []string
	for _, r := range holes(dump, st) {
		got = append(got, r.Tag)
	}
	if fmt.Sprint(got) != "[OLD B D]" {
		t.Fatalf("holes %v, want [OLD B D]", got)
	}
	if h := holes(dump[:1], st); len(h) != 0 {
		t.Fatalf("holes without stored lines: %v", h)
	}
}

func TestReconcileSize(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		prev, seq, buf uint64
		want           int
	}{
		{0, 100, 40000, 40000},     // first time: the whole buffer
		{100, 150, 40000, 2000},    // at least 2000
		{100, 10100, 40000, 20500}, // 2 × new + 500
		{100, 50100, 40000, 40000}, // at most the buffer
		{0, 100, 0, maxResumeHashes},
	} {
		if got := reconcileSize(c.prev, c.seq, c.buf); got != c.want {
			t.Errorf("reconcileSize(%d, %d, %d) = %d, want %d", c.prev, c.seq, c.buf, got, c.want)
		}
	}
}
