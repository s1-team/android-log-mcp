package clock

// SumDeltas is a running sum of real jump deltas for the streaming detector
// path (ClockJump applied before Feed of the first line after the jump).
// For any stamp whose jumps have all been Add'd in arrival order,
// Adj(stamp.T) must equal NewEventClock(those jumps).Continuous(stamp)
// (issue #258; detector ClockJump uses SumDeltas; property vs Continuous).
type SumDeltas struct {
	sum int64
}

// Add accumulates one real jump delta (do not call for "possible").
func (s *SumDeltas) Add(deltaNs int64) {
	if s == nil {
		return
	}
	s.sum += deltaNs
}

// Ns is the running sum (detector jumpNs).
func (s *SumDeltas) Ns() int64 {
	if s == nil {
		return 0
	}
	return s.sum
}

// Adj is device time minus the running sum (detector adj / continuous).
func (s *SumDeltas) Adj(t DeviceInstant) int64 {
	if s == nil {
		return int64(t)
	}
	return int64(t) - s.sum
}
