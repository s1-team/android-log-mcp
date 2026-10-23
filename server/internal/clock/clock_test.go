package clock

import (
	"math/rand"
	"sort"
	"testing"
)

const s = int64(1e9) // 1 second in ns

func jumps(js ...Jump) []Jump { return js }

func TestNewEventClockFiltersPossibleSortsDedupes(t *testing.T) {
	c := NewEventClock(jumps(
		Jump{Seq: 30, DeltaNs: 100, Source: "set"},
		Jump{Seq: 10, DeltaNs: 200, Source: "possible"},
		Jump{Seq: 20, DeltaNs: 300, Source: "lines"},
		Jump{Seq: 30, DeltaNs: 999, Source: "measured"}, // dup Seq: keep first
		Jump{Seq: 5, DeltaNs: 50, Source: ""},           // empty Source = real
	))
	got := c.RealJumps()
	want := []Jump{
		{Seq: 5, DeltaNs: 50, Source: ""},
		{Seq: 20, DeltaNs: 300, Source: "lines"},
		{Seq: 30, DeltaNs: 100, Source: "set"},
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || got[i].DeltaNs != want[i].DeltaNs || got[i].Source != want[i].Source {
			t.Errorf("[%d]=%+v want %+v", i, got[i], want[i])
		}
	}
	// possible must not bump epoch
	if c.EpochAt(15) != 1 { // only Seq 5
		t.Errorf("EpochAt(15)=%d want 1", c.EpochAt(15))
	}
}

func TestEpochAtAndSameEpoch(t *testing.T) {
	c := NewEventClock(jumps(
		Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"},
		Jump{Seq: 40, DeltaNs: -600 * s, Source: "live"},
	))
	cases := []struct {
		seq  ArrivalSeq
		want ClockEpoch
	}{
		{0, 0},
		{19, 0},
		{20, 1},
		{39, 1},
		{40, 2},
		{100, 2},
	}
	for _, tc := range cases {
		if g := c.EpochAt(tc.seq); g != tc.want {
			t.Errorf("EpochAt(%d)=%d want %d", tc.seq, g, tc.want)
		}
	}
	// SameEpoch ↔ procs.sameEpochLocked semantics
	if !c.SameEpoch(10, 15) {
		t.Error("10,15 same epoch")
	}
	if c.SameEpoch(19, 20) {
		t.Error("19,20 cross jump at 20")
	}
	if !c.SameEpoch(20, 39) {
		t.Error("20,39 same epoch")
	}
	if c.SameEpoch(20, 40) {
		t.Error("20,40 cross jump at 40")
	}
	if !c.SameEpoch(0, 40) {
		t.Error("seq 0 is always same (unknown)")
	}
	if !c.SameEpoch(40, 0) {
		t.Error("seq 0 is always same (unknown)")
	}
	// nil clock
	var nilC *EventClock
	if !nilC.SameEpoch(1, 2) || nilC.EpochAt(1) != 0 {
		t.Error("nil EventClock should be epoch 0 / same")
	}
}

func TestContinuousMatchesDetectorAdjAndLaunchesCum(t *testing.T) {
	// Pipeline applies ClockJump(delta) before Feed of the first line after
	// the jump (jumps[0].Seq <= r.Seq). Detector: adj = TimeNs − jumpNs.
	// Launches: cum[seg], seg = count of jumps with Seq <= record.Seq.
	js := jumps(
		Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"},
		Jump{Seq: 40, DeltaNs: -600 * s, Source: "lines"},
	)
	c := NewEventClock(js)

	// Reference cum like launches.go
	cum := make([]int64, len(js)+1)
	for i, j := range js {
		cum[i+1] = cum[i] + j.DeltaNs
	}
	seg := func(seq uint64) int {
		for i, j := range js {
			if j.Seq > seq {
				return i
			}
		}
		return len(js)
	}

	type rec struct {
		seq uint64
		t   int64
	}
	recs := []rec{
		{10, 1_000_000 * s},
		{20, 1_000_000*s + 3600*s + 5*s},
		{30, 1_000_000*s + 3600*s + 50*s},
		{40, 1_000_000*s + 3000*s + 1*s},
		{50, 1_000_000*s + 3000*s + 10*s},
	}

	var run SumDeltas
	applied := 0
	for _, r := range recs {
		// apply jumps whose Seq <= r.seq before "Feed"
		for applied < len(js) && js[applied].Seq <= r.seq {
			run.Add(js[applied].DeltaNs)
			applied++
		}
		st := c.Stamp(ArrivalSeq(r.seq), DeviceInstant(r.t))
		got := c.Continuous(st)
		wantCum := r.t - cum[seg(r.seq)]
		wantAdj := run.Adj(DeviceInstant(r.t))
		if got != wantCum {
			t.Errorf("seq=%d Continuous=%d want cum %d", r.seq, got, wantCum)
		}
		if got != wantAdj {
			t.Errorf("seq=%d Continuous=%d want SumDeltas.Adj %d", r.seq, got, wantAdj)
		}
		if st.Epoch != c.EpochAt(ArrivalSeq(r.seq)) {
			t.Errorf("stamp epoch mismatch")
		}
	}
}

func TestContinuousDeltaSeqZero(t *testing.T) {
	c := NewEventClock(jumps(Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"}))
	a := c.Stamp(10, DeviceInstant(100*s))
	b := c.Stamp(30, DeviceInstant(100*s+3600*s+5*s))
	if d, ok := c.ContinuousDelta(a, b); !ok || d != c.Continuous(b)-c.Continuous(a) {
		t.Fatalf("both seq set: d=%d ok=%v", d, ok)
	}
	z := EventStamp{Seq: 0, T: DeviceInstant(100 * s)}
	if _, ok := c.ContinuousDelta(z, b); ok {
		t.Fatal("seq==0 must ok=false")
	}
	if _, ok := c.ContinuousDelta(a, z); ok {
		t.Fatal("seq==0 must ok=false")
	}
}

func TestShiftCompareMatchesDropboxDelta(t *testing.T) {
	// Reference copy of analysis.dropboxDelta / shiftAfter (issue #93/#152).
	refShiftAfter := func(jumps []Jump, seq uint64) int64 {
		if seq == 0 {
			return 0
		}
		var sum int64
		for _, j := range jumps {
			if j.Seq > seq {
				sum += j.DeltaNs
			}
		}
		return sum
	}
	refDropboxDelta := func(crashNs int64, crashSeq uint64, entryNs int64, jumps []Jump) (d int64, shifted bool) {
		d = abs64(crashNs - entryNs)
		if sh := refShiftAfter(jumps, crashSeq); sh != 0 {
			if ds := abs64(crashNs + sh - entryNs); ds < d {
				return ds, true
			}
		}
		return d, false
	}

	const t0 = int64(1_800_000_000e9)
	fwd := jumps(Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"})
	back := jumps(Jump{Seq: 20, DeltaNs: -3600 * s, Source: "set"})
	two := jumps(
		Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"},
		Jump{Seq: 30, DeltaNs: -600 * s, Source: "lines"},
	)
	// possible must not affect ShiftCompare
	withPossible := append(append([]Jump{}, fwd...), Jump{Seq: 25, DeltaNs: 9999 * s, Source: "possible"})

	type tc struct {
		name     string
		jumps    []Jump
		crashSeq uint64
		entry    int64
	}
	cases := []tc{
		{"no jump", nil, 10, t0 + 5*s},
		{"fwd after", fwd, 10, t0 + 3600*s + 5*s},
		{"fwd raw", fwd, 10, t0 + 5*s},
		{"fwd jump before crash not applied", fwd, 25, t0 + 3600*s + 5*s},
		{"fwd jump at crash line not applied", fwd, 20, t0 + 3600*s + 5*s},
		{"seq 0 no shift", fwd, 0, t0 + 3600*s + 5*s},
		{"back after", back, 10, t0 - 3600*s + 30*s},
		{"two jumps", two, 10, t0 + 3000*s + 7*s},
		{"possible ignored", withPossible, 10, t0 + 3600*s + 5*s},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ref uses only real jumps (as MatchDropbox callers do)
			var real []Jump
			for _, j := range tc.jumps {
				if j.Source != "possible" {
					real = append(real, j)
				}
			}
			wantD, wantS := refDropboxDelta(t0, tc.crashSeq, tc.entry, real)
			c := NewEventClock(tc.jumps)
			gotD, gotS := c.ShiftCompare(c.Stamp(ArrivalSeq(tc.crashSeq), DeviceInstant(t0)), DeviceInstant(tc.entry))
			if gotD != wantD || gotS != wantS {
				t.Fatalf("got (%d,%v) want (%d,%v)", gotD, gotS, wantD, wantS)
			}
		})
	}
}

func TestSumDeltasPropertyMatchesContinuous(t *testing.T) {
	rng := rand.New(rand.NewSource(258))
	for trial := 0; trial < 50; trial++ {
		n := rng.Intn(8)
		var js []Jump
		seq := uint64(10)
		for i := 0; i < n; i++ {
			seq += uint64(1 + rng.Intn(20))
			delta := int64(rng.Intn(7200)-3600) * s
			src := []string{"set", "measured", "live", "lines", "possible"}[rng.Intn(5)]
			js = append(js, Jump{Seq: seq, DeltaNs: delta, Source: src})
		}
		c := NewEventClock(js)
		real := c.RealJumps()

		// walk records at and between jump seqs (sorted unique so SumDeltas stays monotonic)
		seen := map[uint64]bool{1: true}
		var points []uint64
		points = append(points, 1)
		add := func(p uint64) {
			if p == 0 || seen[p] {
				return
			}
			seen[p] = true
			points = append(points, p)
		}
		for _, j := range real {
			add(j.Seq - 1)
			add(j.Seq)
			add(j.Seq + 1)
		}
		if len(real) > 0 {
			add(real[len(real)-1].Seq + 50)
		}
		sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
		var run SumDeltas
		applied := 0
		for _, p := range points {
			for applied < len(real) && real[applied].Seq <= p {
				run.Add(real[applied].DeltaNs)
				applied++
			}
			tDev := DeviceInstant(int64(1_000_000+p) * s)
			st := c.Stamp(ArrivalSeq(p), tDev)
			if got, want := c.Continuous(st), run.Adj(tDev); got != want {
				t.Fatalf("trial=%d seq=%d Continuous=%d SumDeltas.Adj=%d jumps=%+v", trial, p, got, want, real)
			}
		}
	}
}

func TestSameEpochImpliesContinuousDeltaEqualsRaw(t *testing.T) {
	c := NewEventClock(jumps(
		Jump{Seq: 20, DeltaNs: 3600 * s, Source: "set"},
		Jump{Seq: 40, DeltaNs: -600 * s, Source: "set"},
	))
	pairs := [][2]ArrivalSeq{{5, 10}, {20, 30}, {40, 50}, {5, 25}, {25, 45}}
	for _, p := range pairs {
		a := c.Stamp(p[0], DeviceInstant(int64(p[0])*s))
		b := c.Stamp(p[1], DeviceInstant(int64(p[1])*s+100*s))
		d, ok := c.ContinuousDelta(a, b)
		if !ok {
			t.Fatalf("unexpected !ok for %v", p)
		}
		if c.SameEpoch(p[0], p[1]) {
			raw := int64(b.T - a.T)
			if d != raw {
				t.Errorf("SameEpoch %v: ContinuousDelta=%d raw=%d", p, d, raw)
			}
		}
	}
}

func TestNilAndEmptyClock(t *testing.T) {
	c := NewEventClock(nil)
	st := c.Stamp(42, 100)
	if c.Continuous(st) != 100 {
		t.Fatal("empty Continuous")
	}
	d, sh := c.ShiftCompare(st, 90)
	if d != 10 || sh {
		t.Fatalf("empty ShiftCompare %d %v", d, sh)
	}
	var n *EventClock
	if n.Continuous(st) != 100 {
		t.Fatal("nil Continuous")
	}
	if n.RealJumps() != nil {
		t.Fatal("nil RealJumps")
	}
}
