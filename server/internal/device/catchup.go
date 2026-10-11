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

// The state belongs to one recording session (issue #30): a session that
// was stopped (disconnect) can still be returning while the next one has
// begun, so every update names its session and updates from an older
// session are ignored.
type catchState struct {
	mu       sync.Mutex
	session  uint64        // the recording session the state belongs to
	pending  bool          // a recording session is backfilling
	done     chan struct{} // closed when the pending backfill ends
	startSeq uint64        // last stored seq when the backfill began
	tEndNs   int64
	note     string // why catch-up could not be verified, if so
	estTotal uint64 // estimated lines of the whole backfill (0 = unknown)
}

// Progress describes a device's backfill.
type Progress struct {
	// Pending: the backfill is still running (not caught up).
	Pending bool
	// Starting: the device's recording is (re)starting and has not begun
	// reading its buffer yet (e.g. a call during an idle stop; Pending too).
	Starting bool
	// Lines stored since the backfill began; EarliestNs is the oldest of
	// them (0 if none yet).
	Lines      uint64
	EarliestNs int64
	TEndNs     int64
	Note       string
	// EstTotal estimates the lines the backfill stores in all (0 =
	// unknown): when the whole device buffer is read, the entries logd
	// holds in the recorded buffers (`logcat -S`, row "Now") when the
	// stream started. Not estimated for a -T resume.
	EstTotal uint64
}

// catchStart hands the catch-up state to recording session sess and begins
// its backfill (setState: a call right after attach waits for it).
func (d *Device) catchStart(sess uint64) {
	c := &d.catch
	c.mu.Lock()
	c.session = sess
	c.mu.Unlock()
	d.catchBegin(sess)
}

// catchBegin (re)begins the backfill of session sess (each recordOnce).
func (d *Device) catchBegin(sess uint64) {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != sess {
		return // an older session
	}
	if !c.pending {
		c.pending = true
		c.done = make(chan struct{})
	}
	c.startSeq = d.Store.LastSeq()
	c.tEndNs = 0
	c.note = ""
	c.estTotal = 0
}

func (d *Device) catchSetEnd(sess uint64, tEnd int64, note string) {
	c := &d.catch
	c.mu.Lock()
	if c.session == sess {
		c.tEndNs, c.note = tEnd, note
	}
	c.mu.Unlock()
}

// catchDone ends the pending backfill of session sess: caught up, or the
// session ended. An older session's call changes nothing.
func (d *Device) catchDone(sess uint64) {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == sess {
		c.endLocked()
	}
}

// catchStop ends the pending backfill of the current session (stop).
func (d *Device) catchStop() {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked()
}

func (c *catchState) endLocked() {
	if c.pending {
		c.pending = false
		close(c.done)
	}
}

// CatchUp reports the backfill progress.
func (d *Device) CatchUp() Progress {
	c := &d.catch
	c.mu.Lock()
	p := Progress{Pending: c.pending, TEndNs: c.tEndNs, Note: c.note, EstTotal: c.estTotal}
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

// caughtUp is closed when the current backfill ends (closed already when
// none is pending).
func (d *Device) caughtUp() <-chan struct{} {
	c := &d.catch
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending {
		return c.done
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

// BackfillText renders a pending backfill: "backfill in progress (1,423
// of ~27,000 lines; earliest <time>)", or "(1,423 lines so far; ...)"
// without an estimate.
func (p Progress) BackfillText(loc *time.Location) string {
	if p.Starting {
		return "backfill in progress (recording is restarting; nothing read from the device buffer yet)"
	}
	var s string
	if p.EstTotal > 0 {
		s = fmt.Sprintf("backfill in progress (%s of ~%s lines", thousands(p.Lines), thousands(roughly(max(p.EstTotal, p.Lines))))
	} else {
		s = fmt.Sprintf("backfill in progress (%s lines so far", thousands(p.Lines))
	}
	if p.EarliestNs != 0 {
		s += "; earliest " + time.Unix(0, p.EarliestNs).In(loc).Format("2006-01-02 15:04:05.000")
	}
	return s + ")"
}

// roughly rounds an estimate to two significant digits (26,322 → 26,000).
func roughly(n uint64) uint64 {
	unit := uint64(1)
	for n/unit >= 100 {
		unit *= 10
	}
	return (n + unit/2) / unit * unit
}

// thousands formats n with comma separators.
func thousands(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// StatsCmd reads logd's per-buffer statistics (entry counts) of the
// recorded buffers.
const StatsCmd = "logcat -S -b main,system,crash,events"

// parseStatsTotal returns the entries logd holds now, from the "Now" row
// of `logcat -S` ("Now  1784359/18602  705019/4098 ...  2886865/26322"):
// the last column is the total over the listed buffers.
func parseStatsTotal(out string) uint64 {
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || f[0] != "Now" {
			continue
		}
		_, num, ok := strings.Cut(f[len(f)-1], "/")
		if !ok {
			return 0
		}
		n, err := strconv.ParseUint(num, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// measureTotal estimates the backfill's line count (Progress.EstTotal).
func (d *Device) measureTotal(ctx context.Context) {
	sess := sessionOf(ctx)
	if !d.CatchUp().Pending {
		return
	}
	out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, StatsCmd)
	if err != nil {
		return
	}
	if n := parseStatsTotal(out); n > 0 {
		c := &d.catch
		c.mu.Lock()
		if c.pending && c.session == sess {
			c.estTotal = n
		}
		c.mu.Unlock()
	}
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

// StartingProgress is the progress of a device whose recording has not
// started yet (Manager.WaitRecording timed out).
func StartingProgress() Progress { return Progress{Pending: true, Starting: true} }
