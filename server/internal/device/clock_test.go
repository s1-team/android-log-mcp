package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

const day = int64(24 * time.Hour)

// feed gives LiveClock lines dev[i] arriving at host0+i*100ms; returns jumps.
func feedClock(c *LiveClock, seq0 uint64, host0 time.Time, devs []int64) []*store.ClockJump {
	var js []*store.ClockJump
	prev := int64(0)
	for i, d := range devs {
		if j := c.Line(seq0+uint64(i), d, prev, host0.Add(time.Duration(i)*100*time.Millisecond), false); j != nil {
			js = append(js, j)
		}
		prev = d
	}
	return js
}

func TestLiveClockDelayedLineNoJump(t *testing.T) {
	h := time.Unix(1_800_000_000, 0)
	c := &LiveClock{}
	c.Set(0, h, "date")
	at := func(i int) int64 { return h.Add(time.Duration(i) * 100 * time.Millisecond).UnixNano() }
	devs := []int64{at(0), at(1), at(2) - int64(90*time.Second), at(3), at(4)}
	if js := feedClock(c, 1, h, devs); len(js) != 0 {
		t.Fatalf("a single 90 s late line made jumps: %+v", js)
	}
	if off, _, _, _ := c.State(); off > 0 || off < -int64(time.Millisecond) {
		t.Fatalf("offset %v", time.Duration(off))
	}
}

func TestLiveClockJumpAtFirstDeviatingLine(t *testing.T) {
	h := time.Unix(1_800_000_000, 0)
	for _, delta := range []int64{365 * day, -day, -(h.UnixNano() - int64(time.Hour))} { // +1 year, -1 day, back to 1970
		c := &LiveClock{}
		c.Set(0, h, "date")
		var devs []int64
		for i := 0; i < 10; i++ {
			devs = append(devs, h.Add(time.Duration(i)*100*time.Millisecond).UnixNano())
		}
		for i := 10; i < 14; i++ {
			devs = append(devs, h.Add(time.Duration(i)*100*time.Millisecond).UnixNano()+delta)
		}
		js := feedClock(c, 100, h, devs)
		if len(js) != 1 || js[0].Seq != 110 || js[0].Source != "live" || js[0].DeltaNs != delta || js[0].AfterNs != devs[10] || js[0].BeforeNs != devs[9] {
			t.Fatalf("delta %v: jumps %+v", time.Duration(delta), js)
		}
		// the old timeline's larger offsets never win after a back jump
		if off, _, _, _ := c.State(); off != delta {
			t.Fatalf("delta %v: offset %v", time.Duration(delta), time.Duration(off))
		}
	}
}

func TestLiveClockAnnouncedSet(t *testing.T) {
	h := time.Unix(1_800_000_000, 0)
	c := &LiveClock{}
	c.Set(0, h, "date")
	j := c.Line(7, h.UnixNano()+day, h.UnixNano(), h, true)
	if j == nil || j.Seq != 7 || j.DeltaNs != day {
		t.Fatalf("announced set: %+v", j)
	}
}

func testPipe(t *testing.T) (*Pipeline, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "emu-clk", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.UpdateMeta(func(m *store.Meta) {
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-x", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-x"
	})
	p := NewPipeline(st, "b1", time.UTC)
	p.Delay = 0
	p.Clock = &LiveClock{}
	return p, st
}

func rec(ns int64, tag, msg string) *model.Record {
	return &model.Record{TimeNs: ns, PID: 1, TID: 1, Level: 'I', Tag: tag, Msg: msg, Boot: "b1", UID: -1}
}

// replayed lines (catch-up) never set the offset; jumps in them come from
// the lines: an announced set (any size), a step back, a step forward > 1 h
func TestReplayJumpsFromLines(t *testing.T) {
	p, st := testPipe(t)
	h := time.Unix(1_800_000_000, 0)
	p.Clock.Set(0, h, "date")
	t0 := h.UnixNano() - 3*day
	var recs []*model.Record
	add := func(r *model.Record) { recs = append(recs, r) }
	for i := 0; i < 5; i++ {
		add(rec(t0+int64(i)*1e9, "A", "x"))
	}
	add(rec(t0+5e9, "AlarmManagerService", "Setting time of day to sec=123")) // seq 6
	add(rec(t0+5e9+int64(30*time.Minute), "A", "after +30m set"))             // seq 7: "set"
	add(rec(t0+5e9+int64(30*time.Minute)+int64(2*time.Hour)+1e9, "A", "+2h")) // seq 8: possible
	add(rec(t0, "A", "back"))                                                 // seq 9: lines
	add(rec(t0+1e9, "A", "y"))
	for _, r := range recs {
		p.Accept(r, h) // all replayed (Live false)
	}
	if err := p.Drain(true, h); err != nil {
		t.Fatal(err)
	}
	js := st.ClockJumps()
	want := []struct {
		seq uint64
		src string
	}{{7, "set"}, {8, "possible"}, {9, "lines"}}
	if len(js) != len(want) {
		t.Fatalf("jumps %+v", js)
	}
	for i, w := range want {
		if js[i].Seq != w.seq || js[i].Source != w.src {
			t.Fatalf("jump %d: %+v, want %+v", i, js[i], w)
		}
	}
	if off, _, src, _ := p.Clock.State(); off != 0 || src != "date" {
		t.Fatalf("replayed lines moved the offset: %v %s", time.Duration(off), src)
	}
}

// live lines: the two-line rule, the jump at the first deviating line
func TestLivePipelineJump(t *testing.T) {
	p, st := testPipe(t)
	h := time.Unix(1_800_000_000, 0)
	p.Clock.Set(0, h, "date")
	p.Live = true
	for i := 0; i < 6; i++ {
		at := h.Add(time.Duration(i) * time.Second)
		ns := at.UnixNano()
		if i >= 3 {
			ns -= day // set back one day before line 4
		}
		p.Accept(rec(ns, "A", "l"), at)
	}
	if err := p.Drain(true, h.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	js := st.ClockJumps()
	if len(js) != 1 || js[0].Seq != 4 || js[0].Source != "live" || js[0].HostNs != h.Add(3*time.Second).UnixNano() {
		t.Fatalf("jumps %+v", js)
	}
	if off, _, _, _ := p.Clock.State(); off != -day {
		t.Fatalf("offset %v", time.Duration(off))
	}
}

// T_end is matched by its header and copy count: after a clock jump,
// replayed lines dated after T_end are not the end of the replay (issues
// #48, #52 D3); a later agreeing line ends it only after a quiet stream
func TestReplayEndByIdentity(t *testing.T) {
	line := func(sec, us, tid int, msg string) string {
		return fmt.Sprintf("%10d.%06d %5d %5d %5d I %-8s: %s", sec, us, 10146, 100, tid, "T", msg)
	}
	out := line(1_800_000_000, 5, 100, "x") + "\n" + line(1_800_000_001, 7, 100, "dup") + "\n" +
		line(1_800_000_001, 7, 101, "other") + "\n" + line(1_800_000_001, 7, 100, "dup") + "\n" +
		line(1_800_000_001, 7, 100, "dup line 2") + "\n"
	e := parseEnd(out, time.UTC)
	if e.k.ns != 1_800_000_001_000_007_000 || e.k.tid != 100 || e.n != 2 {
		t.Fatalf("end %+v", e)
	}
	if e := parseEnd("", time.UTC); e.k.ns != 0 {
		t.Fatalf("empty: %+v", e)
	}
	h := time.Unix(1_800_000_000, 0)
	tEnd := h.UnixNano() - int64(time.Second)
	if replayEndLate(rec(h.UnixNano()+day, "A", "logged before the set back"), tEnd, h, 0, true, true) {
		t.Fatal("a +1 day replayed line ended the replay")
	}
	agrees := rec(tEnd+int64(time.Second), "A", "after T_end, agrees")
	if replayEndLate(agrees, tEnd, h, 0, true, false) {
		t.Fatal("a later agreeing line inside the burst ended the replay")
	}
	if !replayEndLate(agrees, tEnd, h, 0, true, true) {
		t.Fatal("a later agreeing line after a quiet stream did not end the replay")
	}
	if replayEndLate(rec(tEnd-int64(time.Second), "A", "older"), tEnd, h, 0, true, true) {
		t.Fatal("an older line ended the replay")
	}
}

// reconnect after a jump while disconnected: the connect clock read records
// a "measured" jump at the session's first line; a line-based report of the
// same jump in the replay moves it to the exact line
func TestReconnectAfterJumpWhileDisconnected(t *testing.T) {
	for _, refine := range []bool{false, true} {
		p, st := testPipe(t)
		h := time.Unix(1_800_000_000, 0)
		st.Append([]*model.Record{rec(h.UnixNano(), "A", "before disconnect")})
		d := newDevice(nil, st, "emu-clk")
		d.observeDate(0, h)
		d.observeDate(time.Duration(30*day), h.Add(time.Hour))
		js := st.ClockJumps()
		if len(js) != 1 || js[0].Seq != 2 || js[0].Source != "measured" || js[0].DeltaNs != 30*day {
			t.Fatalf("measured: %+v", js)
		}
		if refine {
			p.Clock = &d.lc
			// replay: lines logged while disconnected, then the set line
			p.Accept(rec(h.UnixNano()+int64(time.Minute), "A", "x"), h.Add(time.Hour))
			p.Accept(rec(h.UnixNano()+int64(2*time.Minute), "AlarmManagerService", "Setting time of day to sec=1"), h.Add(time.Hour))
			p.Accept(rec(h.UnixNano()+30*day+int64(3*time.Minute), "A", "after"), h.Add(time.Hour))
			p.Drain(true, h.Add(time.Hour))
			js = st.ClockJumps()
			if len(js) != 1 || js[0].Seq != 4 || js[0].Source != "set" || js[0].DeltaNs != 30*day {
				t.Fatalf("refined: %+v", js)
			}
		}
	}
}

func TestClockSetLine(t *testing.T) {
	for _, c := range []struct {
		tag, msg string
		ok       bool
		ns       int64
	}{
		{"AlarmManagerService", "Setting time of day to sec=1791794974", true, 1791794974e9},
		{"SystemClockTime", "Setting time of day to sec=1791794974", true, 1791794974e9},
		{"AlarmManagerService", "Setting time of day to sec=1791794974\n", true, 1791794974e9}, // API 5–11
		{"SystemClockTime", "Setting time of day to sec=-5", true, -5e9},
		{"AlarmManager", "Current time only 1000, advancing to build time 1700000000000", true, 1700000000000e6},
		{"SystemClockTime", "Current time only 1000, advancing to build time 1700000000000", true, 1700000000000e6},
		{"AlarmManagerService", "Unable to set rtc to 1791794974: Invalid argument", false, 0}, // not a failure, not an anchor
		{"OtherTag", "Setting time of day to sec=1", false, 0},
		{"SystemClockTime", "Setting time of day to sec=12 extra", false, 0},
	} {
		ns, ok := clockSetLine(rec(0, c.tag, c.msg))
		if ok != c.ok || (ok && ns != c.ns) {
			t.Errorf("%s %q: %v %v", c.tag, c.msg, ns, ok)
		}
	}
}

// with the anchor line one deviating live line is enough (either tag);
// without it (`adb shell date` as root logs nothing) two are needed, and the
// jump is at the first of them either way
func TestLiveJumpWithAndWithoutAnchor(t *testing.T) {
	for _, anchor := range []string{"", "AlarmManagerService", "SystemClockTime"} {
		p, st := testPipe(t)
		h := time.Unix(1_800_000_000, 0)
		p.Clock.Set(0, h, "date")
		p.Live = true
		i := 0
		next := func(off int64, tag, msg string) {
			at := h.Add(time.Duration(i) * time.Second)
			p.Accept(rec(at.UnixNano()+off, tag, msg), at)
			i++
		}
		next(0, "A", "x")
		next(0, "A", "x")
		if anchor != "" {
			next(0, anchor, "Setting time of day to sec=1")
		}
		first := uint64(i + 1)
		next(365*day, "A", "after")
		p.Drain(true, h.Add(time.Minute))
		js := st.ClockJumps()
		if anchor == "" {
			if len(js) != 0 {
				t.Fatalf("no anchor: one deviating line made a jump: %+v", js)
			}
			next(365*day, "A", "after 2")
			p.Drain(true, h.Add(time.Minute))
			js = st.ClockJumps()
		}
		if len(js) != 1 || js[0].Seq != first || js[0].DeltaNs != 365*day || js[0].Source != "live" {
			t.Fatalf("anchor %q: jumps %+v (want seq %d)", anchor, js, first)
		}
	}
}

func TestReplayAnchorSystemClockTime(t *testing.T) {
	p, st := testPipe(t)
	h := time.Unix(1_800_000_000, 0)
	t0 := h.UnixNano() - day
	for k, r := range []*model.Record{rec(t0, "A", "x"), rec(t0+1e9, "SystemClockTime", "Setting time of day to sec=1"), rec(t0+1e9+int64(10*time.Minute), "A", "+10m")} {
		_ = k
		p.Accept(r, h)
	}
	p.Drain(true, h)
	if js := st.ClockJumps(); len(js) != 1 || js[0].Seq != 3 || js[0].Source != "set" {
		t.Fatalf("jumps %+v", js)
	}
}
