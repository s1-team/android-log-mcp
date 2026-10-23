package analysis

import "github.com/s1crab/android-log-mcp/server/internal/clock"

// eventClockOf builds an EventClock from analysis ClockShift DTOs (DropBox /
// ANR). Callers already omit "possible" jumps and apply boot scope when
// needed; NewEventClock still filters Source=="possible" if present.
func eventClockOf(jumps []ClockShift) *clock.EventClock {
	js := make([]clock.Jump, len(jumps))
	for i, j := range jumps {
		js[i] = clock.Jump{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}
	return clock.NewEventClock(js)
}

// eventClockSteps builds an EventClock from launch ClockStep DTOs.
func eventClockSteps(jumps []ClockStep) *clock.EventClock {
	js := make([]clock.Jump, len(jumps))
	for i, j := range jumps {
		js[i] = clock.Jump{Seq: j.Seq, DeltaNs: j.DeltaNs}
	}
	return clock.NewEventClock(js)
}
