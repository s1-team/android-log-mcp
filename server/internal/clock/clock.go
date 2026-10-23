// Package clock is the shared EventClock / clock-segment model (R1, issue #258).
//
// A clock segment (ClockEpoch) is the run of arrivals between two *real*
// device clock jumps. It is not a storage seg-*.jsonl file.
//
// Two named operations are provided and must not be collapsed:
//   - Continuous: detector adj / launches cum (device time minus Σ real
//     jump deltas with Jump.Seq <= stamp.Seq).
//   - ShiftCompare: DropBox dropboxDelta (min of raw and shift-after
//     distances).
//
// "possible" jumps are never epoch boundaries. Phase B migrates analysis /
// procs / launches call sites onto these helpers.
package clock

import "sort"

// ArrivalSeq is dense per-device arrival order (today's Record.Seq).
type ArrivalSeq uint64

// DeviceInstant is a device wall-clock reading in nanoseconds (today's
// TimeNs). Not comparable across epochs by itself.
type DeviceInstant int64

// ClockEpoch is the index of the clock segment: the count of real jumps
// with Jump.Seq <= this arrival. Epoch 0 is before any real jump in the
// table this EventClock was built from.
type ClockEpoch uint32

// EventStamp is the triple analysis should pass through EventClock.
type EventStamp struct {
	Seq   ArrivalSeq
	T     DeviceInstant
	Epoch ClockEpoch
}

// Jump is a device clock jump as EventClock needs it. It mirrors the
// fields of store.ClockJump that matter for epoch math so analysis can
// stay free of a store import. Source == "possible" is excluded by
// NewEventClock; an empty Source is treated as real.
type Jump struct {
	Seq     uint64
	DeltaNs int64
	Source  string
}

// EventClock is a read-only view over a jump table for one boot (or one
// device with a boot filter already applied by the caller).
//
// eventclock: canonical — all analysis clock math should go through this type.
type EventClock struct {
	jumps []Jump // real only, sorted by Seq, unique Seq
}

// NewEventClock builds an EventClock from jumps. It drops Source=="possible",
// sorts by Seq, and keeps the first entry when Seq duplicates.
// Callers that need boot scope (DropBox) should pass only jumps with
// Seq > session.FirstSeq, matching pipeline.dropboxJumps today.
func NewEventClock(jumps []Jump) *EventClock {
	if len(jumps) == 0 {
		return &EventClock{}
	}
	raw := make([]Jump, 0, len(jumps))
	for _, j := range jumps {
		if j.Source == "possible" {
			continue
		}
		raw = append(raw, Jump{Seq: j.Seq, DeltaNs: j.DeltaNs, Source: j.Source})
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Seq < raw[j].Seq })
	out := make([]Jump, 0, len(raw))
	for _, j := range raw {
		if len(out) > 0 && out[len(out)-1].Seq == j.Seq {
			continue // keep first
		}
		out = append(out, j)
	}
	return &EventClock{jumps: out}
}

// RealJumps returns the filtered, sorted real jumps (copy).
func (c *EventClock) RealJumps() []Jump {
	if c == nil || len(c.jumps) == 0 {
		return nil
	}
	out := make([]Jump, len(c.jumps))
	copy(out, c.jumps)
	return out
}

// EpochAt is the count of real jumps with Jump.Seq <= seq.
func (c *EventClock) EpochAt(seq ArrivalSeq) ClockEpoch {
	if c == nil || seq == 0 {
		return 0
	}
	// sort.Search: first i with jumps[i].Seq > seq → count with Seq <= seq
	i := sort.Search(len(c.jumps), func(i int) bool { return c.jumps[i].Seq > uint64(seq) })
	return ClockEpoch(i)
}

// SameEpoch reports whether no real jump lies between a and b (device
// times of the two are comparable). Unknown seq (0): true, matching
// procs.sameEpochLocked — the caller then has only the time to go by.
func (c *EventClock) SameEpoch(a, b ArrivalSeq) bool {
	if a == 0 || b == 0 {
		return true
	}
	return c.EpochAt(a) == c.EpochAt(b)
}

// Stamp builds an EventStamp with EpochAt(seq).
func (c *EventClock) Stamp(seq ArrivalSeq, t DeviceInstant) EventStamp {
	return EventStamp{Seq: seq, T: t, Epoch: c.EpochAt(seq)}
}

// sumDeltasThrough returns Σ DeltaNs for real jumps with Jump.Seq <= seq.
func (c *EventClock) sumDeltasThrough(seq ArrivalSeq) int64 {
	if c == nil || seq == 0 {
		return 0
	}
	n := int(c.EpochAt(seq))
	var sum int64
	for i := 0; i < n; i++ {
		sum += c.jumps[i].DeltaNs
	}
	return sum
}

// shiftAfter returns Σ DeltaNs for real jumps with Jump.Seq > seq.
// seq==0 → 0 (DropBox-only / no place to measure from).
func (c *EventClock) shiftAfter(seq ArrivalSeq) int64 {
	if c == nil || seq == 0 {
		return 0
	}
	var sum int64
	for _, j := range c.jumps {
		if j.Seq > uint64(seq) {
			sum += j.DeltaNs
		}
	}
	return sum
}

// Continuous maps a stamp onto the continuous axis:
// t − Σ{ DeltaNs | Jump.Seq <= s.Seq }. Same as detector adj / launches cum.
// Continuous values must never be printed as device times.
func (c *EventClock) Continuous(s EventStamp) int64 {
	return int64(s.T) - c.sumDeltasThrough(s.Seq)
}

// ContinuousDelta is Continuous(b) − Continuous(a). ok is false when either
// seq is 0 (no continuous axis; DropBox-only paths use ShiftCompare only).
func (c *EventClock) ContinuousDelta(a, b EventStamp) (delta int64, ok bool) {
	if a.Seq == 0 || b.Seq == 0 {
		return 0, false
	}
	return c.Continuous(b) - c.Continuous(a), true
}

// ShiftCompare is the DropBox distance (dropboxDelta): the smaller of the
// raw |anchor.T − other| and the shift-after distance
// |anchor.T + Σ{DeltaNs | Jump.Seq > anchor.Seq} − other|.
// shifted reports that the shift won. seq==0 → no shift (today).
func (c *EventClock) ShiftCompare(anchor EventStamp, other DeviceInstant) (dist int64, shifted bool) {
	d := abs64(int64(anchor.T) - int64(other))
	if sh := c.shiftAfter(anchor.Seq); sh != 0 {
		if ds := abs64(int64(anchor.T) + sh - int64(other)); ds < d {
			return ds, true
		}
	}
	return d, false
}

// ShiftSigned is the signed (other − anchor.T) form of ShiftCompare: raw
// difference, or the one with the anchor moved by shift-after, whichever
// has the smaller absolute value. Used by ANR fact pairing (signedDelta).
func (c *EventClock) ShiftSigned(anchor EventStamp, other DeviceInstant) int64 {
	d := int64(other) - int64(anchor.T)
	if sh := c.shiftAfter(anchor.Seq); sh != 0 {
		if ds := int64(other) - (int64(anchor.T) + sh); abs64(ds) < abs64(d) {
			return ds
		}
	}
	return d
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
