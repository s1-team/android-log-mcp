package device

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Backfill de-duplication after a reconnect compares stored lines as a
// multiset chosen by seq, not by device time (issue #52).

// storedKeys are the stored lines as "device time µs|message", sorted.
func storedKeys(st *store.Store) []string {
	var out []string
	st.Scan(0, st.LastSeq()+1, false, func(r *model.Record) bool {
		out = append(out, fmt.Sprintf("%d|%s", r.TimeNs/1000, r.Msg))
		return true
	})
	sort.Strings(out)
	return out
}

// bufferKeys are the fake device's buffer lines in the same form.
func bufferKeys(t *testing.T, e fakeadb.Env, serial string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.Dir, "dev", serial, "buffer"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		sec, frac, _ := strings.Cut(f[0], ".")
		_, msg, _ := strings.Cut(l, ": ")
		out = append(out, fmt.Sprintf("%s%s|%s", strings.TrimLeft(sec, " "), frac, msg))
	}
	sort.Strings(out)
	return out
}

func reconnect(t *testing.T, e fakeadb.Env, m *Manager, serial string) {
	t.Helper()
	m.Unwant(serial, "test")
	waitFor(t, "fake adb children to exit", 5*time.Second, func() bool { return !e.AnyChildRunning() })
}

func resume(t *testing.T, e fakeadb.Env, m *Manager, serial string) (*Device, string) {
	t.Helper()
	streams := len(e.CallsMatching("-s " + serial + " exec-out logcat -b"))
	d := startDevice(t, m, serial)
	if p := d.WaitCaughtUp(context.Background(), 10*time.Second); p.Pending {
		t.Fatalf("not caught up: %+v", p)
	}
	calls := lastStream(t, e, serial, streams)
	return d, calls[len(calls)-1]
}

// A line logged twice in the same µs is two lines: a resume keeps a third
// copy logged while disconnected, with -T and when the whole buffer is read.
// (Adjacent lines with the same header are one multi-line message, so
// another thread's line sits between the copies.)
func TestSameLineTwiceSameMs(t *testing.T) {
	for _, back := range []bool{false, true} {
		e := fakeEnv(t)
		addDevices(t, e, 3, "FAKEA")
		at := time.Now().Add(-time.Second)
		dup := fakeadb.Line(at, 100, "DUP", "same line")
		other := fakeadb.Line(at, 101, "OTHER", "between")
		e.Log("FAKEA", dup, other, dup)
		m := lazyManager(t, time.Minute)
		d := startDevice(t, m, "FAKEA")
		waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 6 })
		reconnect(t, e, m, "FAKEA")
		e.Log("FAKEA", other, dup)
		if back {
			e.SetDate("FAKEA", at.Add(-time.Hour)) // rollback: whole buffer
		}
		d, stream := resume(t, e, m, "FAKEA")
		if back == strings.Contains(stream, " -T ") {
			t.Fatalf("back=%v: stream %s", back, stream)
		}
		if got, want := storedKeys(d.Store), bufferKeys(t, e, "FAKEA"); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("back=%v: stored %q\nbuffer %q", back, got, want)
		}
	}
}

// D1: the device clock is set back while disconnected and passes the
// newest stored line's device time again before the reconnect. The lines
// dated before it must not be dropped, and the whole buffer is read.
func TestSetBackWhileDisconnectedLosesNothing(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 0, "FAKEA")
	now := time.Now()
	for i := 0; i < 5; i++ {
		e.Log("FAKEA", fakeadb.Line(now.Add(-5*time.Minute+time.Duration(i)*time.Millisecond), 100, "OLD", fmt.Sprintf("old %d", i)))
	}
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	reconnect(t, e, m, "FAKEA")
	e.SetDate("FAKEA", now.Add(-2*time.Minute)) // clock offset −2 min
	for i := 0; i < 3; i++ {
		e.Log("FAKEA", fakeadb.Line(now.Add(-6*time.Minute+time.Duration(i)*time.Millisecond), 100, "BACK", fmt.Sprintf("back %d", i)))
	}
	e.Log("FAKEA", fakeadb.Line(now.Add(-2*time.Minute), 100, "NEW", "passed again"))
	d, stream := resume(t, e, m, "FAKEA")
	if strings.Contains(stream, " -T ") {
		t.Fatalf("resumed with -T after a jump measured at connect: %s", stream)
	}
	if got, want := storedKeys(d.Store), bufferKeys(t, e, "FAKEA"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
	if note := d.RollbackNote(); !strings.Contains(note, "clock jump") || !strings.Contains(note, "skipped 5 already stored") {
		t.Fatalf("note %q", note)
	}
}

// D2: after a live jump below the oldest device time in the buffer, a
// rollback resume must not import the buffer again; the earlier code
// chose the stored lines to compare by device time and found none.
func TestRollbackNoReimport(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
	back := time.Now().Add(-24 * time.Hour)
	e.SetDate("FAKEA", back)
	for i := 0; i < 3; i++ {
		e.Log("FAKEA", fakeadb.Line(back.Add(time.Duration(i)*time.Millisecond), 100, "LIVE", fmt.Sprintf("after -1d %d", i)))
	}
	waitFor(t, "live lines", 5*time.Second, func() bool { return len(messages(d.Store)) == 8 })
	reconnect(t, e, m, "FAKEA")
	e.SetDate("FAKEA", back.Add(-time.Hour)) // device now behind the newest arrival
	d, stream := resume(t, e, m, "FAKEA")
	if strings.Contains(stream, " -T ") {
		t.Fatalf("resumed with -T: %s", stream)
	}
	if got, want := storedKeys(d.Store), bufferKeys(t, e, "FAKEA"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
	if note := d.RollbackNote(); !strings.Contains(note, "went back") || !strings.Contains(note, "skipped 8 already stored") {
		t.Fatalf("note %q", note)
	}
}

// A +1 year jump that is undone does not leave every later reconnect in
// rollback mode: the -T resume point is the newest arrival, not the
// maximum device time.
func TestYearJumpUndoneNoRollback(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 2, "FAKEA")
	m := lazyManager(t, time.Minute)
	d := startDevice(t, m, "FAKEA")
	waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 2 })
	now := time.Now()
	e.SetDate("FAKEA", now.Add(365*24*time.Hour))
	e.Log("FAKEA", fakeadb.Line(now.Add(365*24*time.Hour), 100, "LIVE", "future 1"), fakeadb.Line(now.Add(365*24*time.Hour+time.Millisecond), 100, "LIVE", "future 2"))
	waitFor(t, "future lines", 5*time.Second, func() bool { return len(messages(d.Store)) == 4 })
	e.SetDate("FAKEA", now.Add(time.Second))
	e.Log("FAKEA", fakeadb.Line(now.Add(time.Second), 100, "LIVE", "undone 1"), fakeadb.Line(now.Add(time.Second+time.Millisecond), 100, "LIVE", "undone 2"))
	waitFor(t, "lines after the undo", 5*time.Second, func() bool { return len(messages(d.Store)) == 6 })
	reconnect(t, e, m, "FAKEA")
	d, _ = resume(t, e, m, "FAKEA")
	if note := d.RollbackNote(); strings.Contains(note, "went back") {
		t.Fatalf("rollback mode after an undone jump: %q", note)
	}
	if got, want := storedKeys(d.Store), bufferKeys(t, e, "FAKEA"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored %q\nbuffer %q", got, want)
	}
}

// For each jump size, a live jump then the jump undone while
// disconnected: right when the resume reports caught up (D3), the stored
// lines equal the device buffer as a multiset.
func TestReconnectAfterEachJumpSize(t *testing.T) {
	day := 24 * time.Hour
	for _, j := range []time.Duration{-365 * day, -day, -time.Hour, -90 * time.Second, -30 * time.Second,
		30 * time.Second, 90 * time.Second, time.Hour, day, 365 * day} {
		t.Run(j.String(), func(t *testing.T) {
			e := fakeEnv(t)
			addDevices(t, e, 5, "FAKEA")
			m := lazyManager(t, time.Minute)
			d := startDevice(t, m, "FAKEA")
			waitFor(t, "backfill", 10*time.Second, func() bool { return len(messages(d.Store)) == 5 })
			now := time.Now()
			e.SetDate("FAKEA", now.Add(j))
			dup := fakeadb.Line(now.Add(j), 100, "LIVE", "jumped")
			e.Log("FAKEA", dup, fakeadb.Line(now.Add(j), 101, "LIVE", "between"), dup, fakeadb.Line(now.Add(j+time.Millisecond), 100, "LIVE", "jumped 2"))
			waitFor(t, "live lines", 5*time.Second, func() bool { return len(messages(d.Store)) == 9 })
			reconnect(t, e, m, "FAKEA")
			e.SetDate("FAKEA", now.Add(time.Second))
			away := fakeadb.Line(now.Add(time.Second), 100, "AWAY", "while disconnected")
			e.Log("FAKEA", away, fakeadb.Line(now.Add(time.Second), 101, "AWAY", "between"), away)
			d, _ = resume(t, e, m, "FAKEA")
			if got, want := storedKeys(d.Store), bufferKeys(t, e, "FAKEA"); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("stored %q\nbuffer %q", got, want)
			}
		})
	}
}
