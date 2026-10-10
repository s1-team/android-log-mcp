package timeparse

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 10, 2, 30, 0, 0, loc)
	cases := map[string]string{
		"30m":                       "2026-10-10 02:00:00",
		"2h":                        "2026-10-10 00:30:00",
		"1h30m":                     "2026-10-10 01:00:00",
		"1d":                        "2026-10-09 02:30:00",
		"2026-10-10 01:20:00":       "2026-10-10 01:20:00",
		"2026-10-10 01:20:00.250":   "2026-10-10 01:20:00",
		"01:20:00":                  "2026-10-10 01:20:00",
		"01:20":                     "2026-10-10 01:20:00",
		"2026-10-09T17:20:00Z":      "2026-10-10 01:20:00",
		"2026-10-10T01:20:00+08:00": "2026-10-10 01:20:00",
		"2026-10-10T01:20:00":       "2026-10-10 01:20:00",
		"now":                       "2026-10-10 02:30:00",
	}
	for in, want := range cases {
		got, err := Parse("from", in, now)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.In(loc).Format("2006-01-02 15:04:05") != want {
			t.Errorf("%s => %s want %s", in, got.In(loc).Format("2006-01-02 15:04:05"), want)
		}
	}
	if _, err := Parse("from", "yesterday 3pm", now); err == nil {
		t.Error("expected error")
	}
}
