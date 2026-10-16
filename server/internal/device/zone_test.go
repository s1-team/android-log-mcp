package device

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// `date +%z` is the source of truth; persist.sys.timezone is used only when it agrees.
func TestResolveZone(t *testing.T) {
	t.Parallel()
	at := time.Unix(1791572000, 0)
	l, label := ResolveZone("Asia/Kuching", "+0800", at)
	if l == nil || l.String() != "Asia/Kuching" || label != "Asia/Kuching" {
		t.Fatalf("consistent zone: %v %q", l, label)
	}
	l, label = ResolveZone("America/New_York", "+0800", at)
	if _, off := at.In(l).Zone(); off != 8*3600 || !strings.Contains(label, "disagrees") {
		t.Fatalf("mismatch must fall back to the offset: %v %q", l, label)
	}
	l, _ = ResolveZone("", "-0330", at)
	if _, off := at.In(l).Zone(); off != -(3*3600 + 1800) {
		t.Fatalf("offset only: %v", l)
	}
	if l, _ := ResolveZone("", "", at); l != nil {
		t.Fatal("nothing known must return nil")
	}
}

// The named zone is checked against `date +%z` at the device's own time, not
// the host's: a device on standard time and a host on daylight time (or the
// reverse) must not make a consistent persist.sys.timezone look wrong (#107).
func TestProbeDeviceNowDecidesZoneName(t *testing.T) {
	t.Parallel()
	dev := time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC) // winter in New York: -0500
	p := &Probe{Props: map[string]string{"now": strconv.FormatInt(dev.Unix(), 10) + ".000"}, T0: time.Now(), T1: time.Now()}
	at := probeDeviceNow(p, false)
	if d := at.Sub(dev); d < -time.Second || d > time.Second {
		t.Fatalf("probeDeviceNow = %v, want the device clock reading %v", at, dev)
	}
	l, label := ResolveZone("America/New_York", "-0500", at)
	if l == nil || label != "America/New_York" {
		t.Fatalf("device time in winter: %v %q", l, label)
	}
	// the same inputs evaluated at a host time in summer (-0400) disagree
	if _, label := ResolveZone("America/New_York", "-0500", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); !strings.Contains(label, "disagrees") {
		t.Fatalf("test premise: host-summer time must disagree, got %q", label)
	}
	// no device clock reading: host now
	if got := probeDeviceNow(&Probe{Props: map[string]string{}}, false); time.Since(got) > time.Minute {
		t.Fatalf("fallback = %v", got)
	}
}
