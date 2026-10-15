package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/store"
	"github.com/s1crab/android-log-mcp/server/internal/timeparse"
)

// Two clocks (issue #48, docs/glossary.md): device time is the Android
// device's clock (the timestamps of logcat lines; every time-valued
// parameter and every line uses it); host time is the clock of the machine
// running the MCP / service process. Device now is estimated from the
// newest device clock observation (device.ClockInfo).

// hostTime formats a host time in the host timezone, labelled.
func hostTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

// fmtDur is a compact signed duration: +365d 0h00m, -2h03m05s, +0.04s.
func fmtDur(d time.Duration, signed bool) string {
	sign := ""
	if signed {
		sign = "+"
	}
	if d < 0 {
		sign, d = "-", -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%s%.2fs", sign, d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%s%dm%02ds", sign, int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%s%dh%02dm%02ds", sign, int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	days := int(d.Hours()) / 24
	return fmt.Sprintf("%s%dd %dh%02dm", sign, days, int(d.Hours())%24, int(d.Minutes())%60)
}

// clockState explains how device now was obtained.
func clockState(ci device.ClockInfo) string {
	s := ""
	switch ci.State {
	case "observed":
		src := "device clock read"
		if ci.Source == "live line" {
			src = "live line"
		}
		s = fmt.Sprintf("observed: %s %s ago", src, fmtAgo(ci.HostNow.Sub(ci.ObservedAt)))
	case "estimated":
		s = fmt.Sprintf("estimated: last device clock reading %s ago, at host time %s", fmtAgo(ci.HostNow.Sub(ci.ObservedAt)), hostTime(ci.ObservedAt))
	default:
		s = "unknown: no device clock reading; relative times count back from the newest stored line"
	}
	if ci.Err != "" {
		s += "; auto_time " + ci.Err
	}
	return s
}

func fmtAgo(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f s", max(d.Seconds(), 0))
	}
	return strings.TrimPrefix(fmtDur(d, false), "+")
}

// clockLine: device now, host now and the clock offset, compact (every
// query response); statusLine is the detailed form for logcat_status.
func clockLine(ci device.ClockInfo) string {
	st := ci.State
	switch ci.State {
	case "estimated":
		st = "estimated, observed " + fmtAgo(ci.HostNow.Sub(ci.ObservedAt)) + " ago"
	case "unknown":
		st = "unknown: newest stored line"
	}
	return fmt.Sprintf("device now %s (%s) · host now %s · clock offset %s",
		ci.Now.Format("2006-01-02 15:04:05"), st, hostTime(ci.HostNow), fmtDur(ci.Offset, true))
}

func statusLine(ci device.ClockInfo) string {
	return fmt.Sprintf("device now %s (%s) · host now %s · clock offset (device − host) %s",
		ci.Now.Format("2006-01-02 15:04:05.000"), clockState(ci), hostTime(ci.HostNow), fmtDur(ci.Offset, true))
}

// jumpWord describes a clock jump.
func jumpWord(j store.ClockJump) string {
	switch {
	case j.Source == "measured":
		w := "device clock jumped forward "
		if j.DeltaNs < 0 {
			w = "device clock set back "
		}
		return w + fmtDur(time.Duration(abs64(j.DeltaNs)), false) + " (measured at reconnect; position within the replayed lines unknown)"
	case j.Source == "possible":
		return "device time steps forward " + fmtDur(time.Duration(j.DeltaNs), false) + " (a forward clock jump or a quiet period)"
	case j.DeltaNs < 0:
		return "device clock set back " + fmtDur(time.Duration(-j.DeltaNs), false)
	}
	return "device clock jumped forward " + fmtDur(time.Duration(j.DeltaNs), false)
}

// clockKV are the clock fields of a response header (#85): device now in
// ISO-8601 with offset, clock=observed|estimated|unknown (enum), the offset
// in seconds and, for observed/estimated, the age of the reading. Host now
// is printed only with verbose (glossary: host time never selects lines).
func clockKV(ci device.ClockInfo, verbose bool) []any {
	kv := []any{"dev_now", isoSec(ci.Now), "clock", ci.State, "offset_s", secs(ci.Offset)}
	if ci.State == "observed" || ci.State == "estimated" {
		kv = append(kv, "clock_age_s", int(max(ci.HostNow.Sub(ci.ObservedAt).Seconds(), 0)))
	}
	if verbose {
		kv = append(kv, "host_now", isoSec(ci.HostNow))
	}
	return kv
}

// jumpLines is the `clock_jump` line of one jump (verbose detail; the
// default response carries the one-line summary of clockSummary). before /
// after: matches on each side (-1: not counted).
func jumpLines(j store.ClockJump, loc *time.Location, before, after int, afterCursor string) string {
	hostISO := "-"
	if j.HostNs != 0 {
		hostISO = time.Unix(0, j.HostNs).Local().Format(time.RFC3339)
	}
	kv := []any{"seq", j.Seq, "delta_s", secs(time.Duration(j.DeltaNs)), "source", j.Source,
		"device_before", isoOrDash(j.BeforeNs, loc), "device_after", isoOrDash(j.AfterNs, loc), "host", hostISO}
	if before >= 0 {
		kv = append(kv, "matches_before", before, "matches_after", after)
	}
	l := hdr("clock_jump", kv...)
	if afterCursor != "" {
		l += nextClause(param("after", afterCursor))
	}
	return l + "\n"
}

func isoOrDash(ns int64, loc *time.Location) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, ns).In(loc).Format("2006-01-02T15:04:05.000Z07:00")
}

// window parses from/to in device time (issue #48): host: values are
// rejected, and a relative from (or defRel when from is empty)
// without to ends at device now + relSlack. Zero: unbounded side.
func window(d *device.Device, from, to string, defRel time.Duration) (ci device.ClockInfo, fromNs, toNs int64, err error) {
	if timeparse.HostPrefixed(from) {
		return ci, 0, 0, hostRejected("from", from, d.Clock())
	}
	if timeparse.HostPrefixed(to) {
		return ci, 0, 0, hostRejected("to", to, d.Clock())
	}
	rel := timeparse.IsRelative(from) || (from == "" && defRel > 0)
	ci = d.Clock() // from the stream: no device clock read per query
	if from != "" {
		tm, err := timeparse.Parse("from", from, ci.Now)
		if err != nil {
			return ci, 0, 0, errf("%v", err)
		}
		fromNs = tm.UnixNano()
	} else if defRel > 0 {
		fromNs = ci.Now.Add(-defRel).UnixNano()
	}
	if to != "" {
		tm, err := timeparse.Parse("to", to, ci.Now)
		if err != nil {
			return ci, 0, 0, errf("%v", err)
		}
		toNs = tm.UnixNano()
	} else if rel {
		toNs = ci.Now.Add(relSlack).UnixNano()
	}
	return ci, fromNs, toNs, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
