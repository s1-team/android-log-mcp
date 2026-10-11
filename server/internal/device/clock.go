package device

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Device now (issue #48, comment 6107308483): host now + the clock offset
// (device time − host time). The clock offset comes from
//   - one device clock read (`date +%s.%N`) per recording session, in the
//     probe at connect;
//   - live lines (arrived after catch-up): lineOff = device time − host
//     arrival time. Latency only lowers lineOff, so the offset is the
//     highest lineOff of the live lines of the last 10 s since the last
//     jump. Two consecutive live lines that disagree by more than
//     store.ClockJumpThreshold in the same direction are a clock jump at the
//     first of them; one alone is a delayed line.
// There is no periodic clock polling and no clock read per query: every
// clock set is logged at once (AlarmManagerService "Setting time of day",
// TIME_SET receivers), so a quiet stream means an unchanged offset.

// ClockInfo describes device now.
type ClockInfo struct {
	Now        time.Time     // device now, in the device timezone
	HostNow    time.Time     // host now (host timezone)
	Offset     time.Duration // device time − host time
	ObservedAt time.Time     // host time of the newest observation (zero: none)
	Source     string        // "date" | "live line" | "" (none)
	// State: "observed" (recording, observation ≤ 10 s old), "estimated"
	// (older observation, e.g. a quiet or disconnected device) or
	// "unknown" (never observed: counting back from the newest stored line).
	State    string
	AutoTime string // "1", "0" or "" (unknown)
	Err      string // why the last auto_time read failed ("read failed: …")
}

// observedFor: an observation this recent makes device now "observed".
const observedFor = 10 * time.Second

// liveWindow: live lines this recent (host arrival) set the offset.
const liveWindow = 10 * time.Second

type clockObs struct {
	at  time.Time
	off int64
}

// pendJump is a live line that disagreed with the offset, waiting for a
// second one to confirm a jump.
type pendJump struct {
	seq     uint64
	off     int64
	at      time.Time
	prevDev int64 // device time of the line before it
	dev     int64
}

// LiveClock tracks the clock offset (safe for concurrent use).
type LiveClock struct {
	mu   sync.Mutex
	ok   bool
	off  int64
	at   time.Time
	src  string
	win  []clockObs // observations since the last jump within liveWindow
	pend *pendJump
}

// State returns the offset, when and how it was observed.
func (c *LiveClock) State() (off int64, at time.Time, src string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off, c.at, c.src, c.ok
}

// Set takes a device clock read (or a stored offset with src "").
func (c *LiveClock) Set(off int64, at time.Time, src string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ok, c.off, c.at, c.src, c.pend = true, off, at, src, nil
	c.win = []clockObs{{at, off}}
}

func (c *LiveClock) recompute(at time.Time) {
	k := 0
	best := int64(0)
	for _, o := range c.win {
		if at.Sub(o.at) <= liveWindow {
			if k == 0 || o.off > best {
				best = o.off
			}
			c.win[k] = o
			k++
		}
	}
	c.win = c.win[:k]
	if k > 0 {
		c.off = best
	}
}

// Line feeds one live line (seq, device time, previous line's device time,
// host arrival). announced: the line before it was a clock set
// ("Setting time of day"), which confirms a jump at once. It returns the
// clock jump it confirms, if any.
func (c *LiveClock) Line(seq uint64, dev, prevDev int64, at time.Time, announced bool) *store.ClockJump {
	c.mu.Lock()
	defer c.mu.Unlock()
	lineOff := dev - at.UnixNano()
	if !c.ok {
		c.ok, c.off, c.at, c.src = true, lineOff, at, "live line"
		c.win = []clockObs{{at, lineOff}}
		return nil
	}
	th := store.ClockJumpThreshold
	d := lineOff - c.off
	if d <= th && d >= -th {
		c.pend = nil
		c.win = append(c.win, clockObs{at, lineOff})
		c.recompute(at)
		c.at, c.src = at, "live line"
		return nil
	}
	p := c.pend
	if announced {
		p = &pendJump{seq: seq, off: lineOff, at: at, prevDev: prevDev, dev: dev}
	} else if p == nil || (p.off-c.off > 0) != (d > 0) || lineOff-p.off > th || p.off-lineOff > th {
		c.pend = &pendJump{seq: seq, off: lineOff, at: at, prevDev: prevDev, dev: dev}
		return nil
	}
	// confirmed: the jump is at the first disagreeing line; only lines
	// from it on set the offset (the old timeline must not win the max)
	j := &store.ClockJump{Seq: p.seq, HostNs: p.at.UnixNano(), BeforeNs: p.prevDev, AfterNs: p.dev, Source: "live"}
	c.win = []clockObs{{p.at, p.off}, {at, lineOff}}
	old := c.off
	c.recompute(at)
	j.DeltaNs = c.off - old
	c.at, c.src, c.pend = at, "live line", nil
	return j
}

// Clock returns device now and how it was obtained.
func (d *Device) Clock() ClockInfo {
	hostNow := time.Now()
	off, at, src, ok := d.lc.State()
	d.mu.RLock()
	loc := d.loc
	live := d.recording && d.state == "device"
	auto, aerr := d.autoTime, d.autoErr
	d.mu.RUnlock()
	if loc == nil {
		loc = time.Local
	}
	if d.Store != nil {
		m := d.Store.Meta()
		if m.ClockHostNs > at.UnixNano() && !d.Store.IsWriter() {
			// a follower process: the recorder's newer observation
			off, at, src, ok = m.ClockOffsetNs, time.Unix(0, m.ClockHostNs), "date", true
			live = hostNow.Sub(at) <= observedFor
		}
		if auto == "" {
			auto = m.AutoTime
		}
	}
	ci := ClockInfo{HostNow: hostNow, Offset: time.Duration(off), ObservedAt: at, Source: src, AutoTime: auto, Err: aerr}
	if !ok || at.IsZero() || at.UnixNano() <= 0 {
		ci.ObservedAt, ci.Source, ci.State = time.Time{}, "", "unknown"
		ci.Now = hostNow.In(loc)
		ci.Offset = 0
		if d.Store != nil {
			if n := d.Store.Stats().NewestNs; n != 0 {
				ci.Now = time.Unix(0, n).In(loc)
				ci.Offset = ci.Now.Sub(hostNow)
			}
		}
		return ci
	}
	ci.Now = hostNow.Add(time.Duration(off)).In(loc)
	ci.State = "estimated"
	if live && hostNow.Sub(at) <= observedFor {
		ci.State = "observed"
	}
	return ci
}

// Now is device now (see Clock).
func (d *Device) Now() time.Time { return d.Clock().Now }

// observeDate takes the device clock read of the probe at connect. A
// change by more than the jump threshold against the previous offset (the
// device was disconnected or not recorded meanwhile) is a "measured" jump
// at the session's first line; the replay may refine its position.
//
// auto_time is not read here (a slow `settings` call would delay the
// start of recording): it is marked stale, and the next logcat_status reads
// it once (RefreshAutoTime).
func (d *Device) observeDate(off time.Duration, at time.Time) {
	prev, prevAt, _, had := d.lc.State()
	d.lc.Set(int64(off), at, "date")
	d.mu.Lock()
	d.autoStale = true
	d.mu.Unlock()
	if d.Store == nil || !d.Store.IsWriter() {
		return
	}
	if had && prevAt.UnixNano() > 0 {
		if delta := int64(off) - prev; delta > store.ClockJumpThreshold || delta < -store.ClockJumpThreshold {
			d.Store.AddClockJump(store.ClockJump{Seq: d.Store.LastSeq() + 1, HostNs: at.UnixNano(), DeltaNs: delta,
				BeforeNs: at.UnixNano() + prev, AfterNs: at.UnixNano() + int64(off), Source: "measured"})
		}
	}
	d.saveClock(true)
}

// saveClock writes the offset to meta for follower processes: on a change
// (force) or at most every 10 s.
func (d *Device) saveClock(force bool) {
	if d.Store == nil || !d.Store.IsWriter() {
		return
	}
	off, at, _, ok := d.lc.State()
	if !ok {
		return
	}
	d.mu.Lock()
	due := force || at.Sub(d.clockSaved) >= observedFor
	if due {
		d.clockSaved = at
	}
	auto := d.autoTime
	d.mu.Unlock()
	if !due {
		return
	}
	d.Store.UpdateMeta(func(m *store.Meta) {
		m.ClockOffsetNs, m.ClockHostNs = off, at.UnixNano()
		if auto != "" {
			m.AutoTime = auto
		}
	})
}

// onClockEvent: the pipeline saw a clock jump or an auto_time change:
// auto_time is re-read lazily (AutoTime) and the offset saved.
func (d *Device) onClockEvent() {
	d.mu.Lock()
	d.autoStale = true
	d.mu.Unlock()
	d.saveClock(true)
}

// RefreshAutoTime re-reads auto_time when it is stale (a clock jump or an
// auto_time change was logged since the last read) and this process
// records the connected device: one adb call, 1 s timeout.
func (d *Device) RefreshAutoTime(ctx context.Context) {
	d.mu.Lock()
	ok := d.autoStale && d.owned && d.recording && d.state == "device" && d.adb != nil
	if ok {
		d.autoStale = false
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := d.adb.Shell(ctx, d.Addr(), time.Second, "settings get global auto_time 2>/dev/null")
	v := strings.TrimSpace(out)
	d.mu.Lock()
	switch {
	case err != nil:
		d.autoErr, d.autoStale = "read failed: "+err.Error(), true
	case v == "0" || v == "1":
		d.autoTime, d.autoErr = v, ""
	}
	d.mu.Unlock()
	if err == nil {
		d.saveClock(true)
	}
}

// clockOffset is the current offset estimate (ok=false: never observed).
func (d *Device) clockOffset() (int64, bool) {
	off, _, _, ok := d.lc.State()
	return off, ok
}
