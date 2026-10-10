package device

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Catch-up (issue #11 §5): when a device starts recording, the recorder
// first measures T_end, the time of the newest line in the device buffer
// (`logcat -d -t 1` over the recorded buffers). The device is caught up
// once a streamed line with time ≥ T_end has been stored or deduplicated.
// No timing heuristics: an empty buffer, or a -T resume whose T_end is not
// newer than the last stored line, is caught up at once.

// CatchUpWait is the longest a log-tool call waits for catch-up.
var CatchUpWait = 10 * time.Second

// TEndCmd measures T_end (the same buffers the recorder streams).
const TEndCmd = "logcat -d -t 1 -b main,system,crash,events -v epoch"

type catchState struct {
	mu       sync.Mutex
	pending  bool          // a recording session is backfilling
	done     chan struct{} // closed when the pending backfill ends
	startSeq uint64        // last stored seq when the backfill began
	tEndNs   int64
	note     string // why catch-up could not be verified, if so
}

// Progress describes a device's backfill.
type Progress struct {
	// Pending: the backfill is still running (not caught up).
	Pending bool
	// Lines stored since the backfill began; EarliestNs is the oldest of
	// them (0 if none yet).
	Lines      uint64
	EarliestNs int64
	TEndNs     int64
	Note       string
}

func (d *Device) catchBegin() {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pending {
		c.pending = true
		c.done = make(chan struct{})
	}
	c.startSeq = d.Store.LastSeq()
	c.tEndNs = 0
	c.note = ""
}

func (d *Device) catchSetEnd(tEnd int64, note string) {
	c := &d.catch
	c.mu.Lock()
	c.tEndNs, c.note = tEnd, note
	c.mu.Unlock()
}

// catchDone ends the pending backfill: caught up, or the session ended.
func (d *Device) catchDone() {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending {
		c.pending = false
		close(c.done)
	}
}

// CatchUp reports the backfill progress.
func (d *Device) CatchUp() Progress {
	c := &d.catch
	c.mu.Lock()
	p := Progress{Pending: c.pending, TEndNs: c.tEndNs, Note: c.note}
	start := c.startSeq
	c.mu.Unlock()
	if last := d.Store.LastSeq(); last > start {
		p.Lines = last - start
		if r := d.Store.Get(start + 1); r != nil {
			p.EarliestNs = r.TimeNs
		}
	}
	return p
}

// WaitCaughtUp waits until the device is caught up, at most timeout.
func (d *Device) WaitCaughtUp(ctx context.Context, timeout time.Duration) Progress {
	c := &d.catch
	c.mu.Lock()
	pending, done := c.pending, c.done
	c.mu.Unlock()
	if pending {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return d.CatchUp()
}

// BackfillText renders a pending backfill ("backfill in progress (N lines,
// earliest <time>)").
func (p Progress) BackfillText(loc *time.Location) string {
	s := fmt.Sprintf("backfill in progress (%d lines", p.Lines)
	if p.EarliestNs != 0 {
		s += ", earliest " + time.Unix(0, p.EarliestNs).In(loc).Format("2006-01-02 15:04:05.000")
	}
	return s + ")"
}

// parseEpochTimes returns the times of the `-v epoch` lines in out.
func parseEpochTimes(out string) []int64 {
	var ts []int64
	for _, ln := range strings.Split(out, "\n") {
		m := epochLine.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		frac := (m[2] + "000000000")[:9]
		ns, _ := strconv.ParseInt(frac, 10, 64)
		ts = append(ts, sec*1e9+ns)
	}
	return ts
}

// measureEnd runs TEndCmd: the newest buffered line's time (0 = empty).
func (d *Device) measureEnd(ctx context.Context) (int64, error) {
	out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, TEndCmd)
	if err != nil {
		return 0, err
	}
	var end int64
	for _, t := range parseEpochTimes(out) {
		end = max(end, t)
	}
	return end, nil
}

// measureStart returns the oldest buffered line's time (0 = unknown).
func (d *Device) measureStart(ctx context.Context) int64 {
	out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, "logcat -d -b main,system,crash,events -v epoch | head -n 8")
	if err != nil {
		return 0
	}
	var start int64
	for _, t := range parseEpochTimes(out) {
		if start == 0 || t < start {
			start = t
		}
	}
	return start
}

// rollbackSet collects the hashes of stored records of the current boot
// session that may still be in the device buffer (time ≥ bufStart, or
// everything when bufStart is unknown), bounded.
func rollbackSet(st *store.Store, bufStart int64) map[uint64]bool {
	const maxHashes = 500000
	set := map[uint64]bool{}
	first := uint64(0)
	if m := st.Meta(); len(m.Sessions) > 0 {
		first = m.Sessions[len(m.Sessions)-1].FirstSeq
	}
	last := st.LastSeq()
	if first > 0 {
		first--
	}
	st.Scan(first, last+1, true, func(r *model.Record) bool {
		if bufStart != 0 && r.TimeNs < bufStart-int64(10*time.Second) {
			return false
		}
		if bufStart == 0 || r.TimeNs >= bufStart {
			set[recHash(r)] = true
		}
		return len(set) < maxHashes
	})
	return set
}

// RollbackNote describes the last clock rollback (empty if none).
func (d *Device) RollbackNote() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.rollback == "" {
		return ""
	}
	if n := d.rollbackSkipped.Load(); n > 0 {
		return fmt.Sprintf("%s; skipped %d already stored lines", d.rollback, n)
	}
	return d.rollback
}

func (d *Device) noteRollback(lastNs, nowNs int64, loc *time.Location) {
	msg := fmt.Sprintf("device clock went back: the last stored line (%s) is later than the device clock (%s); read the whole buffer without -T and dropped lines already stored",
		time.Unix(0, lastNs).In(loc).Format("2006-01-02 15:04:05.000"), time.Unix(0, nowNs).In(loc).Format("2006-01-02 15:04:05.000"))
	log.Printf("%s: %s", d.Serial, msg)
	d.rollbackSkipped.Store(0)
	d.mu.Lock()
	d.rollback = msg
	d.mu.Unlock()
}
