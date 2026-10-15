package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// refCrashes is the pre-index LoadCrashes: decode the whole file, newest
// version of each id, in first-appearance order, reversed: newest arrival
// first (issue #55: device time is not an order).
func refCrashes(st *store.Store) []*analysis.Crash {
	byID := map[string]*analysis.Crash{}
	var order []string
	for _, b := range st.ReadAux("crashes") {
		c := new(analysis.Crash)
		if json.Unmarshal(b, c) != nil || c.ID == "" {
			continue
		}
		if _, ok := byID[c.ID]; !ok {
			order = append(order, c.ID)
		}
		byID[c.ID] = c
	}
	out := make([]*analysis.Crash, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func slim(c *analysis.Crash) analysis.Crash {
	x := *c
	x.Body, x.Traces, x.CPU, x.Dropbox, x.Tombstone = "", "", "", "", ""
	if len(x.Sources) == 0 {
		x.Sources = nil
	}
	return x
}

func checkIndex(t *testing.T, st *store.Store, what string) {
	t.Helper()
	want := refCrashes(st)
	got := CrashSummaries(st)
	if len(got) != len(want) {
		t.Fatalf("%s: %d summaries, want %d", what, len(got), len(want))
	}
	for i := range want {
		g, w := slim(got[i]), slim(want[i])
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("%s: summary %d = %+v, want %+v", what, i, g, w)
		}
	}
	full := LoadCrashes(st)
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("%s: LoadCrashes differs from a full decode", what)
	}
	for _, w := range want {
		if f := CrashFull(st, w.ID)[0]; !reflect.DeepEqual(f, w) {
			t.Fatalf("%s: CrashFull(%s) = %+v, want %+v", what, w.ID, f, w)
		}
	}
}

func testCrash(i, version int, base int64) *analysis.Crash {
	return &analysis.Crash{ID: fmt.Sprintf("cr%d", i), Kind: "java", Boot: "b1", TimeNs: base + int64(i%7)*1e9 - int64(i)*1e6,
		Seq: uint64(i + 1), PID: 1000 + i, Process: "com.example.app", Package: "com.example.app",
		Headline:  fmt.Sprintf("java.lang.IllegalStateException: boom %d v%d", i, version),
		Body:      fmt.Sprintf("java.lang.IllegalStateException: boom %d\n\tat com.example.A.b(A.java:%d)\n\"quoted\" \\u00e9 é", i, version),
		Signature: fmt.Sprintf("sig%d", i%3), Sources: []string{"logcat"}, Traces: map[bool]string{true: "trace v" + fmt.Sprint(version)}[version > 0]}
}

func TestCrashIndexMatchesFullDecode(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkIndex(t, st, "empty")
	base := time.Now().UnixNano()
	for i := 0; i < 30; i++ {
		st.AppendAux("crashes", testCrash(i, 0, base))
		if i%5 == 0 {
			checkIndex(t, st, fmt.Sprintf("after %d", i))
		}
	}
	for i := 0; i < 30; i += 3 { // newer versions supersede
		st.AppendAux("crashes", testCrash(i, 1, base))
	}
	st.AppendAux("crashes", json.RawMessage(`{"kind":"java"}`)) // no id: ignored
	checkIndex(t, st, "versions")

	// a partial last line is not indexed until it is complete
	p := filepath.Join(st.Dir, "crashes.jsonl")
	line, _ := json.Marshal(testCrash(99, 0, base))
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	f.Write(line[:20])
	f.Close()
	if n := len(CrashSummaries(st)); n != 30 {
		t.Fatalf("partial line indexed: %d", n)
	}
	f, _ = os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	f.Write(append(line[20:], '\n'))
	f.Close()
	checkIndex(t, st, "completed line")

	// a read-only store of the same directory shares the index
	ro, err := store.OpenReadOnly(st.Dir, store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	checkIndex(t, ro, "read-only store")
}

func TestCrashIndexReloadsAfterCompaction(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for i := 0; i < 20; i++ {
		c := testCrash(i, 0, now.UnixNano())
		if i < 5 {
			c.TimeNs = now.Add(-3 * time.Hour).UnixNano() // dropped by compaction
		}
		st.AppendAux("crashes", c)
		st.AppendAux("crashes", testCrash(i, 1, now.UnixNano()-map[bool]int64{true: int64(3 * time.Hour)}[i < 5]))
	}
	checkIndex(t, st, "before compaction")
	if saved := CompactAux(st, now); saved <= 0 {
		t.Fatalf("compaction saved nothing")
	}
	checkIndex(t, st, "after compaction")
	if n := len(CrashSummaries(st)); n != 15 {
		t.Fatalf("after compaction %d crashes, want 15", n)
	}
	// appends after the rewrite are picked up incrementally
	st.AppendAux("crashes", testCrash(50, 0, now.UnixNano()))
	checkIndex(t, st, "append after compaction")

	// a rewrite to the same size with other content is detected too
	lines := st.ReadAux("crashes")
	for i, l := range lines {
		var c analysis.Crash
		json.Unmarshal(l, &c)
		c.Headline = c.Headline[:len(c.Headline)-1] + "X"
		lines[i], _ = json.Marshal(&c)
	}
	if err := st.RewriteAux("crashes", lines); err != nil {
		t.Fatal(err)
	}
	checkIndex(t, st, "same-size rewrite")
}

// Ingest stays linear: emitting n crashes decodes O(n) index lines, not
// O(n²) (each EmitCrash used to re-read the whole file twice).
func TestEmitCrashLinear(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	const n = 400
	before := crashLinesIndexed.Load()
	base := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		c := testCrash(i, 0, base)
		c.ID = ""
		p.EmitCrash(c)
		if i%4 == 0 { // a later version of an earlier crash
			c2 := *c
			c2.Headline += " (updated)"
			p.EmitCrash(&c2)
		}
	}
	got := crashLinesIndexed.Load() - before
	if lines := int64(len(st.ReadAux("crashes"))); got > 2*lines {
		t.Fatalf("indexed %d lines for %d stored lines (quadratic re-read?)", got, lines)
	}
	if len(CrashSummaries(st)) != n {
		t.Fatalf("%d crashes, want %d", len(CrashSummaries(st)), n)
	}
	checkIndex(t, st, "after ingest")
}

// The recorder appends while tool readers list and decode (go test -race).
func TestCrashIndexConcurrentReaders(t *testing.T) {
	st, err := store.Open(t.TempDir(), "d", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	base := time.Now().UnixNano()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				sums := CrashSummaries(st)
				for i := 1; i < len(sums); i++ {
					if idNum(sums[i].ID) > idNum(sums[i-1].ID) {
						t.Errorf("summaries not newest arrival first")
						return
					}
				}
				if len(sums) > 0 {
					c := CrashFull(st, sums[len(sums)/2].ID)[0]
					if c == nil || c.Body == "" {
						t.Errorf("full decode of %s failed", sums[len(sums)/2].ID)
						return
					}
					// callers copy before changing a crash (MatchDropbox does)
					cp := *sums[0]
					cp.Sources = append(cp.Sources, fmt.Sprint("reader", r))
				}
				if r == 0 {
					LoadCrashes(st)
				}
			}
		}(r)
	}
	for i := 0; i < 300; i++ {
		c := testCrash(i, 0, base)
		c.ID = ""
		p.EmitCrash(c)
		if i == 150 {
			CompactAux(st, time.Now())
		}
	}
	close(done)
	wg.Wait()
	checkIndex(t, st, "after concurrent ingest")
}

// idNum: the arrival number of an id "crN" / "anrN" (ids are assigned in
// arrival order).
func idNum(id string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(id, "abcdefghijklmnopqrstuvwxyz"))
	return n
}
