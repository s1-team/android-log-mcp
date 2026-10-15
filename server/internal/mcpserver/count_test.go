package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #20: counting stops countBudget after the page is full (a lower
// bound with the counted span); a search without a filter or time window
// gets the exact total from the store's record range.

// bigFixture stores n records, one per second ending now, every 10th with
// tag Ten (level warn), the rest tag Other; small segments, blocks and
// in-memory window so counts cross all of them.
func bigFixture(t *testing.T, n int) (*Tools, *store.Store) {
	t.Helper()
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 256 << 10, BlockBytes: 16 << 10, MemRecords: 1000}
	st, err := store.Open(base, "emu-1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ = "emu-1", "UTC"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
	})
	t0 := time.Now().Add(-time.Duration(n) * time.Second)
	var recs []*model.Record
	for i := 0; i < n; i++ {
		r := &model.Record{TimeNs: t0.Add(time.Duration(i) * time.Second).UnixNano(), PID: 100, TID: 100, Level: 'I', Tag: "Other", Msg: fmt.Sprintf("line %d", i), Boot: "b1"}
		if i%10 == 0 {
			r.Tag, r.Level = "Ten", 'W'
		}
		recs = append(recs, r)
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	ro, err := store.OpenReadOnly(st.Dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ro.Close)
	return &Tools{M: m}, ro
}

var countLine = regexp.MustCompile(`^(?:ok|empty) .*? matched=(\d+) exact=([01])(?: (counted_from|counted_to)=(\S+))? `)

// parseCount reads matched / exact / counted_from|to from the header line.
func parseCount(t *testing.T, out string) (total string, exact bool, key, at string) {
	t.Helper()
	m := countLine.FindStringSubmatch(firstLine(out))
	if m == nil {
		t.Fatalf("no matched= exact= in the header of:\n%s", out)
	}
	return m[1], m[2] == "1", m[3], m[4]
}

func withCountBudget(t *testing.T, d time.Duration) {
	old := countBudget
	countBudget = d
	t.Cleanup(func() { countBudget = old })
}

// No filter: the exact total without scanning past the page, also for a
// before cursor (paging) and with a tiny count budget.
func TestSearchExactTotalWithoutFilter(t *testing.T) {
	// serial: sets countBudget (package global)
	withCountBudget(t, time.Nanosecond)
	tl, _ := bigFixture(t, 30000)
	out, e := call(t, tl, "logcat_search", `{"limit":50}`)
	must(t, out, e, "matched=30000 exact=1 shown=50 ", "\nsummary of=")
	if total, exact, key, _ := parseCount(t, out); total != "30000" || !exact || key != "" {
		t.Fatalf("count line: %s %v %s\n%s", total, exact, key, out)
	}
	i := strings.Index(out, `before="`)
	cur := out[i+8:]
	cur = cur[:strings.IndexByte(cur, '"')]
	out, e = call(t, tl, "logcat_search", `{"limit":50,"before":"`+cur+`"}`)
	must(t, out, e, "matched=29950 exact=1 shown=50 ")
	out, e = call(t, tl, "logcat_search", `{"limit":50,"after":"`+cur+`"}`)
	must(t, out, e, "matched=49 exact=1 shown=49 ")
}

// With a filter, counting stops after the budget: a lower bound, the span
// it covers, what to do for an exact count, and the fields.
func TestSearchPartialCount(t *testing.T) {
	// serial: sets countBudget (package global)
	withCountBudget(t, time.Nanosecond)
	tl, _ := bigFixture(t, 30000)
	out, e := call(t, tl, "logcat_search", `{"filter":"tag=:Ten","limit":20}`)
	must(t, out, e, " exact=0 counted_from=", " shown=20 ", "\ntruncated older=", " exact=0 ", "\nsummary of=")
	total, exact, key, at := parseCount(t, out)
	if exact || key != "counted_from" || at == "" || total == "3000" {
		t.Fatalf("count line: %s %v %s %s\n%s", total, exact, key, at, out)
	}
	// a budget long enough for the whole range: exact, as before
	withCountBudget(t, time.Minute)
	out, e = call(t, tl, "logcat_search", `{"filter":"tag=:Ten","limit":20}`)
	must(t, out, e, "matched=3000 exact=1 shown=20 ", "\nsummary of=2000 tags=Ten:100% msgs=2000x\"Ten: line #\"")
}

// Fewer matches than the limit: the whole range is scanned (exact), as
// before; zero matches carry the count line too.
func TestSearchFewMatchesExact(t *testing.T) {
	// serial: sets countBudget (package global)
	withCountBudget(t, time.Nanosecond)
	tl, _ := bigFixture(t, 30000)
	out, e := call(t, tl, "logcat_search", `{"filter":"message:\"line 7\" tag=:Other","limit":1000}`)
	must(t, out, e, " exact=1 ")
	out, e = call(t, tl, "logcat_search", `{"filter":"message:zz-none"}`)
	must(t, out, e, "empty view=lines ", " matched=0 exact=1 scanned=")
}
