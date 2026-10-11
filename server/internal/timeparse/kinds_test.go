package timeparse

import "testing"

func TestKinds(t *testing.T) {
	for _, c := range []struct {
		s              string
		rel, now, host bool
	}{
		{"5m", true, true, false},
		{"1h30m", true, true, false},
		{"now", true, true, false},
		{"15:00", false, true, false},
		{"15:00:01.5", false, true, false},
		{"2026-10-10 15:00:00", false, false, false},
		{"2026-10-10T15:00:00+08:00", false, false, false},
		{"host:5m", false, false, true},
		{" HOST:2026-10-10 15:00", false, false, true},
		{"", false, false, false},
	} {
		if IsRelative(c.s) != c.rel || NeedsNow(c.s) != c.now || HostPrefixed(c.s) != c.host {
			t.Errorf("%q: rel %v now %v host %v", c.s, IsRelative(c.s), NeedsNow(c.s), HostPrefixed(c.s))
		}
	}
}
