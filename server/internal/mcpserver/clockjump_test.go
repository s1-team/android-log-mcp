package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Synthetic store with device clock jumps (issues #21, #48). Lines are in
// arrival order; every line's message is "m<index>".
type jumpFx struct {
	tools  *Tools
	recs   []*model.Record // arrival order
	devNow time.Time       // device now when the fixture was built
}

type seg struct {
	start time.Time
	n     int
	step  time.Duration
}

const yearD = 365 * 24 * time.Hour

func newJumpFx(t *testing.T, devNow time.Time, segs []seg) *jumpFx {
	t.Helper()
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 64 << 10, BlockBytes: 4 << 10, MemRecords: 200}
	st, err := store.Open(base, "emu-j", cfg)
	if err != nil {
		t.Fatal(err)
	}
	hostNow := time.Now()
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ = "emu-j", "UTC"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
		m.ClockOffsetNs, m.ClockHostNs = int64(devNow.Sub(hostNow)), hostNow.UnixNano()
	})
	fx := &jumpFx{devNow: devNow}
	var jumps []store.ClockJump
	for si, s := range segs {
		var recs []*model.Record
		for i := 0; i < s.n; i++ {
			k := len(fx.recs) + len(recs)
			recs = append(recs, &model.Record{TimeNs: s.start.Add(time.Duration(i) * s.step).UnixNano(), PID: 100, TID: 100, Level: 'I', Tag: "T", Msg: fmt.Sprintf("m%d", k), Boot: "b1", UID: -1})
		}
		if err := st.Append(recs); err != nil {
			t.Fatal(err)
		}
		if si > 0 && len(fx.recs) > 0 && len(recs) > 0 {
			prev := fx.recs[len(fx.recs)-1]
			if d := recs[0].TimeNs - prev.TimeNs; d < store.ClockJumpThreshold && d > -store.ClockJumpThreshold {
				fx.recs = append(fx.recs, recs...)
				continue
			}
			jumps = append(jumps, store.ClockJump{Seq: recs[0].Seq, DeltaNs: recs[0].TimeNs - prev.TimeNs, BeforeNs: prev.TimeNs, AfterNs: recs[0].TimeNs, HostNs: hostNow.UnixNano(), Source: "live"})
		}
		fx.recs = append(fx.recs, recs...)
	}
	for _, j := range jumps {
		st.AddClockJump(j)
	}
	st.Close()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	fx.tools = &Tools{M: m}
	return fx
}

func (fx *jumpFx) call(t *testing.T, name, args string) string {
	t.Helper()
	out, isErr := fx.tools.Call(context.Background(), name, json.RawMessage(args))
	if isErr {
		t.Fatalf("%s %s: error: %s", name, args, out)
	}
	return out
}

// brute counts lines with from <= device time < to (0: unbounded).
func (fx *jumpFx) brute(from, to int64) int {
	n := 0
	for _, r := range fx.recs {
		if (from == 0 || r.TimeNs >= from) && (to == 0 || r.TimeNs < to) {
			n++
		}
	}
	return n
}

var reMsgID = regexp.MustCompile(`: m(\d+)$`)

func msgIDs(out string) []int {
	var ids []int
	for _, ln := range strings.Split(out, "\n") {
		if m := reMsgID.FindStringSubmatch(ln); m != nil {
			n, _ := strconv.Atoi(m[1])
			ids = append(ids, n)
		}
	}
	return ids
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// the expiry-testing timeline: normal, +1 year, back to 1970, +1 day,
// back to just before device now, two lines around device now + 2 s
func expiryFx(t *testing.T) *jumpFx {
	devNow := time.Now().UTC().Truncate(time.Second).Add(-time.Hour) // device 1 h behind host
	return newJumpFx(t, devNow, []seg{
		{devNow.Add(-2 * time.Hour), 600, 10 * time.Second}, // ends devNow-20m
		{devNow.Add(yearD), 100, time.Second},
		{time.Unix(1000, 0), 100, time.Second},
		{devNow.Add(24 * time.Hour), 50, time.Second},
		{devNow.Add(-4 * time.Minute), 200, time.Second}, // ends devNow-40s
		{devNow.Add(time.Second + 500*time.Millisecond), 1, time.Second},
		{devNow.Add(5 * time.Second), 1, time.Second},
	})
}

func TestClockJumpFullScanEquality(t *testing.T) {
	t.Parallel()
	fx := expiryFx(t)
	n := fx.devNow
	for _, w := range [][2]time.Time{
		{n.Add(-3 * time.Hour), n},
		{n.Add(-3 * time.Hour), n.Add(48 * time.Hour)},
		{n.Add(yearD), n.Add(yearD + 50*time.Second)},
		{time.Unix(1000, 0), time.Unix(1050, 0)},
		{time.Unix(0, 0), time.Unix(100000, 0)},
		{n.Add(24 * time.Hour), n.Add(25 * time.Hour)},
		{time.Unix(0, 0), n.Add(2 * yearD)},
		{n.Add(-5 * time.Minute), n.Add(10 * time.Second)},
	} {
		args := fmt.Sprintf(`{"from":%q,"to":%q,"limit":1000}`, iso(w[0]), iso(w[1]))
		out := fx.call(t, "logcat_search", args)
		total, exact, _, _ := parseCount(t, out)
		want := fx.brute(w[0].UnixNano(), w[1].UnixNano())
		if total != strconv.Itoa(want) || !exact {
			t.Errorf("%s: total %s exact %v, want %d", args, total, exact, want)
		}
		if ids := msgIDs(out); len(ids) != min(want, 1000) && !strings.Contains(out, "[output cap") {
			t.Errorf("%s: %d lines shown, want %d", args, len(ids), min(want, 1000))
		}
		sum := fx.call(t, "logcat_search", fmt.Sprintf(`{"from":%q,"to":%q,"view":"summary"}`, iso(w[0]), iso(w[1])))
		if want > 0 && !strings.Contains(sum, fmt.Sprintf("total %s lines", commas(want))) {
			t.Errorf("summary %s: want total %d in:\n%s", args, want, sum)
		}
	}
}

// relative windows end at device now + 2 s; lines from the future
// timelines are excluded and counted in a note; to= or {} is unbounded
func TestRelativeWindowUpperBound(t *testing.T) {
	t.Parallel()
	fx := expiryFx(t)
	want := 200 + 1 // devNow-4m..-40s, and the line at devNow+1.5s (not +5s)
	for _, args := range []string{`{"filter":"age:5m","limit":1000}`, `{"from":"5m","limit":1000}`} {
		out := fx.call(t, "logcat_search", args)
		if total, exact, _, _ := parseCount(t, out); total != strconv.Itoa(want) || !exact {
			t.Errorf("%s: total %s, want %d:\n%s", args, total, want, out)
		}
		if !strings.Contains(out, "clock: 151 matching lines are dated after device now") {
			t.Errorf("%s: future note missing:\n%s", args, out)
		}
		if !strings.Contains(out, "device now ") || !strings.Contains(out, "host now ") {
			t.Errorf("%s: header without device now / host now:\n%s", args, out)
		}
	}
	// explicit to: no +2 s bound
	out := fx.call(t, "logcat_search", fmt.Sprintf(`{"from":"5m","to":%q,"limit":1000}`, iso(fx.devNow.Add(time.Minute))))
	if total, _, _, _ := parseCount(t, out); total != "202" {
		t.Errorf("from=5m with to: total %s, want 202", total)
	}
	// {}: unbounded, newest by arrival (the +5 s line last), jumps marked
	out = fx.call(t, "logcat_search", `{"limit":1000}`)
	if total, exact, _, _ := parseCount(t, out); total != strconv.Itoa(len(fx.recs)) || !exact {
		t.Errorf("{}: total %s, want %d", total, len(fx.recs))
	}
	ids := msgIDs(out)
	if len(ids) == 0 || ids[len(ids)-1] != len(fx.recs)-1 {
		t.Errorf("{}: last line shown is not the newest by arrival: %v", ids[max(0, len(ids)-3):])
	}
	if c := strings.Count(out, "\nclock_jump: "); c != 3 || !strings.Contains(out, "clock: 4 device clock jumps in the scanned range; 4 split the matches; the newest 3") {
		t.Errorf("{}: %d clock_jump lines:\n%s", c, out)
	}
}

// every line is reached exactly once by paging with before= cursors,
// across all jumps; the same with a device-time window covering all
func TestMoreWalkExactlyOnce(t *testing.T) {
	t.Parallel()
	fx := expiryFx(t)
	reMore := regexp.MustCompile(`more: before="([^"]+)"`)
	for _, base := range []string{`"limit":37`, fmt.Sprintf(`"limit":37,"from":%q,"to":%q`, iso(time.Unix(0, 0)), iso(fx.devNow.Add(2*yearD)))} {
		seen := map[int]int{}
		args := "{" + base + "}"
		for page := 0; page < 100; page++ {
			out := fx.call(t, "logcat_search", args)
			ids := msgIDs(out)
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				seen[id]++
			}
			m := reMore.FindStringSubmatch(out)
			if m == nil {
				break
			}
			args = fmt.Sprintf(`{%s,"before":%q}`, base, m[1])
		}
		if len(seen) != len(fx.recs) {
			t.Errorf("%s: reached %d of %d lines", base, len(seen), len(fx.recs))
		}
		for id, c := range seen {
			if c != 1 {
				t.Errorf("%s: m%d seen %d times", base, id, c)
			}
		}
	}
}

func TestHostPrefixRejected(t *testing.T) {
	t.Parallel()
	fx := expiryFx(t)
	for _, c := range []struct{ args, want string }{
		{`{"from":"host:5m"}`, "from: host time is not supported: from/to/age: use device time"},
		{`{"to":"host:2026-10-10 15:00"}`, "to: host time is not supported"},
		{`{"filter":"tag:T age:host:5m"}`, "age:host:5m: age: always counts back from device now ("},
	} {
		out, isErr := fx.tools.Call(context.Background(), "logcat_search", json.RawMessage(c.args))
		if !isErr || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v %q", c.args, isErr, out)
		}
		if !strings.Contains(c.args, "age:") && !strings.Contains(out, "take the cursor from logcat_status") {
			t.Errorf("%s: no cursor advice: %q", c.args, out)
		}
	}
	out, isErr := fx.tools.Call(context.Background(), "logcat_app_launches", json.RawMessage(`{"from":"host:5m"}`))
	if !isErr || !strings.Contains(out, "host time is not supported") {
		t.Errorf("launches host: %q", out)
	}
}

// a back jump just before the query (device now already post-jump): the
// relative window returns the post-jump lines, not the old timeline
func TestRelativeRightAfterBackJump(t *testing.T) {
	t.Parallel()
	devNow := time.Now().UTC().Add(-24 * time.Hour) // clock just set back one day
	fx := newJumpFx(t, devNow, []seg{
		{devNow.Add(24*time.Hour - 10*time.Minute), 100, time.Second}, // old timeline
		{devNow.Add(-20 * time.Second), 10, time.Second},              // post-jump
	})
	for _, args := range []string{`{"filter":"age:5m"}`, `{"from":"5m"}`} {
		out := fx.call(t, "logcat_search", args)
		ids := msgIDs(out)
		if total, _, _, _ := parseCount(t, out); total != "10" || len(ids) != 10 || ids[0] != 100 {
			t.Errorf("%s: total %s ids %v:\n%s", args, total, ids, out)
		}
	}
}
