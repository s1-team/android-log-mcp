package store

import (
	"testing"
	"time"
)

// a clock read at reconnect ("measured", at the session's first line) is
// moved to a line-based report of the same jump, not to a different one
func TestMeasuredJumpRefine(t *testing.T) {
	day := int64(24 * time.Hour)
	for _, c := range []struct {
		measured, lines int64
		refined         bool
	}{
		{30 * day, 30*day + int64(time.Minute), true},
		{364 * day, 29 * day, false}, // two jumps while disconnected
		{-day, -day - int64(2*time.Second), true},
		{-day, day, false},
	} {
		s, err := Open(t.TempDir(), "x", Config{})
		if err != nil {
			t.Fatal(err)
		}
		s.Append(mk(10, time.Now().UnixNano()))
		s.AddClockJump(ClockJump{Seq: 3, DeltaNs: c.measured, Source: "measured"})
		s.AddClockJump(ClockJump{Seq: 7, DeltaNs: c.lines, Source: "lines"})
		js := s.ClockJumps()
		if c.refined && (len(js) != 1 || js[0].Seq != 7 || js[0].DeltaNs != c.measured || js[0].Source != "lines") {
			t.Errorf("%+v: %+v", c, js)
		}
		if !c.refined && (len(js) != 2 || js[0].Seq != 3) {
			t.Errorf("%+v: %+v", c, js)
		}
		s.Close()
	}
}
