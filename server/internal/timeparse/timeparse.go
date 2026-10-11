// Package timeparse parses from/to parameters in device-local terms.
package timeparse

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var reRel = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m|h|d)$`)

// Parse interprets s relative to deviceNow (whose Location is the device zone).
// Accepted: "30m"/"2h"/"1d"/"1h30m" (= now minus that), "2026-10-10 01:20:00[.mmm]",
// "01:20:00" or "01:20" (device's today), ISO-8601 with or without offset.
func Parse(param, s string, deviceNow time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	loc := deviceNow.Location()
	if s == "" {
		return time.Time{}, fmt.Errorf("%s is empty", param)
	}
	if strings.EqualFold(s, "now") {
		return deviceNow, nil
	}
	if d, ok := parseRel(s); ok {
		return deviceNow.Add(-d), nil
	}
	for _, f := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999 -0700"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.In(loc), nil
		}
	}
	for _, f := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	for _, f := range []string{"15:04:05.999999999", "15:04"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			y, m, d := deviceNow.Date()
			return time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc), nil
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 1e9 {
		return time.Unix(0, int64(n*1e9)).In(loc), nil
	}
	return time.Time{}, fmt.Errorf(`%s=%q not understood. Use "2026-10-10 15:00:00", "15:00:00" (device's today), ISO-8601 like "2026-10-10T15:00:00+08:00", or a duration like "30m" / "2h" (= device now minus that)`, param, s)
}

func parseRel(s string) (time.Duration, bool) {
	if m := reRel.FindStringSubmatch(s); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		return time.Duration(f * float64(unit)), true
	}
	if strings.ContainsAny(s, "hms") && !strings.ContainsAny(s, ":-T ") {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d, true
		}
	}
	return 0, false
}

// IsRelative: s counts back from device now ("now", "30m", "1h30m").
func IsRelative(s string) bool {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return true
	}
	_, ok := parseRel(s)
	return ok
}

// NeedsNow: s depends on device now: relative, or a time of day on the
// device's today ("15:00").
func NeedsNow(s string) bool {
	if IsRelative(s) {
		return true
	}
	s = strings.TrimSpace(s)
	for _, f := range []string{"15:04:05.999999999", "15:04"} {
		if _, err := time.Parse(f, s); err == nil {
			return true
		}
	}
	return false
}

// HostPrefixed: s asks for host time ("host:5m", "host:2026-10-10 15:00").
func HostPrefixed(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 5 && strings.EqualFold(s[:5], "host:")
}
