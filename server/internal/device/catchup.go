package device

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Catch-up (issue #11 §5): when a device starts recording, the recorder
// first measures T_end, the time of the newest line in the device buffer
// (`logcat -d -t 1` over the recorded buffers). The device is caught up
// once a streamed line with time ≥ T_end has been stored or deduplicated.
// No timing heuristics: an empty buffer, or a -T resume whose T_end is not
// newer than the last stored line, is caught up at once.

// DefaultCatchUpWait is the longest a log-tool call waits for catch-up
// (Manager.CatchUpWait).
const DefaultCatchUpWait = 10 * time.Second

// TEndCmd measures T_end (the same buffers the recorder streams).
const TEndCmd = "logcat -d -t 50 -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable"

// endKey identifies the T_end line by its header (device time at µs, pid,
// tid, level, tag): a multi-line message repeats it on every line.
type endKey struct {
	ns       int64
	pid, tid int
	level    model.Level
	tag      string
}

func keyOf(r *model.Record) endKey {
	return endKey{r.TimeNs, r.PID, r.TID, r.Level, r.Tag}
}

// endMark is the replay's end: the n-th streamed record with key k (copies
// logged in the same µs by the same thread count; issue #52 D3).
type endMark struct {
	k endKey
	n int
}

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

// measureEnd runs TEndCmd: the newest buffered record by arrival (zero
// key = empty buffer) and how many of the last 50 lines' records share its
// key.
func (d *Device) measureEnd(ctx context.Context, loc *time.Location) (endMark, error) {
	out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, TEndCmd)
	if err != nil {
		return endMark{}, err
	}
	return parseEnd(out, loc), nil
}

func parseEnd(out string, loc *time.Location) endMark {
	asm := &logcat.Assembler{Loc: loc, Year: time.Now().Year()}
	var recs []*model.Record
	for _, ln := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		if r := asm.Push(strings.TrimSuffix(ln, "\r")); r != nil {
			recs = append(recs, r)
		}
	}
	if r := asm.Flush(); r != nil {
		recs = append(recs, r)
	}
	if len(recs) == 0 {
		return endMark{}
	}
	e := endMark{k: keyOf(recs[len(recs)-1])}
	for _, r := range recs {
		if keyOf(r) == e.k {
			e.n++
		}
	}
	return e
}

// multiset counts the hashes of stored lines (recHash) that the replayed
// part of a stream can repeat. A replayed line is dropped only while its
// hash has a count left, so a line the app really logged twice in the
// same ms survives (issue #52).
type multiset map[uint64]int32

// take consumes one count of h; false if none is left.
func (m multiset) take(h uint64) bool {
	if m[h] > 0 {
		m[h]--
		return true
	}
	return false
}

// maxResumeHashes caps a multiset (≈ 20 MB).
const maxResumeHashes = 500000

// resumeSlack: stored lines this far before the -T resume point are still
// compared (logd interleaves its buffers by device time).
const resumeSlack = 10 * time.Second

// sessionFirst is the seq before the current boot session's first line.
func sessionFirst(st *store.Store) uint64 {
	m := st.Meta()
	if len(m.Sessions) == 0 {
		return 0
	}
	if f := m.Sessions[len(m.Sessions)-1].FirstSeq; f > 0 {
		return f - 1
	}
	return 0
}

// tailMultiset counts the last n stored lines of the current boot session,
// chosen by seq, not by device time: after a clock jump device time says
// nothing about what is still in the device buffer (issue #52 D2).
func tailMultiset(st *store.Store, n int) multiset {
	set := multiset{}
	if n > maxResumeHashes || n <= 0 {
		n = maxResumeHashes
	}
	seen := 0
	var runs splitRuns
	st.Scan(sessionFirst(st), st.LastSeq()+1, true, func(r *model.Record) bool {
		set[recHash(r)]++
		runs.add(set, r)
		seen++
		return seen < n
	})
	runs.end(set)
	return set
}

// boundaryMultiset counts the stored lines that logcat -T arrNs can
// repeat: newest first by seq, back to the first line dated more than
// resumeSlack before arrNs. Used only when no clock jump lies among the
// lines still in the device buffer, so device time follows arrival order.
func boundaryMultiset(st *store.Store, arrNs int64) multiset {
	set := multiset{}
	if arrNs == 0 {
		return set
	}
	seen := 0
	var runs splitRuns
	st.Scan(sessionFirst(st), st.LastSeq()+1, true, func(r *model.Record) bool {
		if r.TimeNs < arrNs-int64(resumeSlack) {
			return false
		}
		set[recHash(r)]++
		runs.add(set, r)
		seen++
		return seen < maxResumeHashes
	})
	runs.end(set)
	return set
}

// splitRuns finds a multi-line entry that was stored as several records
// (issue #192): the live assembler flushes a pending entry after 150 ms
// without a line, so on a slow transport the rest of the entry arrives later
// and becomes a record of its own with the same header. The replay after a
// reconnect assembles the entry whole, and its hash matches none of the
// pieces. For every run of consecutive stored records with one header, add()
// also counts the hash the whole entry would have (the messages joined by
// "\n", as the assembler joins them), so the replayed entry is recognised.
// Records arrive newest first (the scans above).
type splitRuns struct {
	head string   // header of the run
	msgs []string // messages, newest first
	r0   model.Record
}

func runHeader(r *model.Record) string {
	return fmt.Sprintf("%d|%d|%d|%c|%s|%d|%s", r.TimeNs, r.PID, r.TID, r.Level, r.Tag, r.UID, r.Buffer)
}

func (s *splitRuns) add(set multiset, r *model.Record) {
	h := runHeader(r)
	if len(s.msgs) > 0 && h != s.head {
		s.end(set)
	}
	if len(s.msgs) == 0 {
		s.head, s.r0 = h, *r
	}
	s.msgs = append(s.msgs, r.Msg)
}

// end closes the current run, counting its whole-entry hash if it has
// several records.
func (s *splitRuns) end(set multiset) {
	if n := len(s.msgs); n > 1 {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			s.msgs[i], s.msgs[j] = s.msgs[j], s.msgs[i]
		}
		whole := s.r0
		whole.Msg = strings.Join(s.msgs, "\n")
		set[recHash(&whole)]++
	}
	s.msgs = s.msgs[:0]
}

// bufferLines returns the entries logd holds in the recorded buffers
// (`logcat -S`; 0 = unknown).
func (d *Device) bufferLines(ctx context.Context) uint64 {
	out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, StatsCmd)
	if err != nil {
		return 0
	}
	return parseStatsTotal(out)
}

// tailSize is how many stored lines may still be in a device buffer of n
// entries: n × 1.5 (lines logged since the read), capped; the cap when n
// is unknown.
func tailSize(n uint64) int {
	if n == 0 || n*3/2 > maxResumeHashes {
		return maxResumeHashes
	}
	return int(n * 3 / 2)
}

// jumpInTail reports whether a recorded clock jump lies after seq from
// (among the stored lines that may still be in the device buffer, or
// measured at this connect).
func jumpInTail(st *store.Store, from uint64) bool {
	for _, j := range st.ClockJumps() {
		if j.Seq > from {
			return true
		}
	}
	return false
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
	d.noteWhole(fmt.Sprintf("device clock went back: the newest stored line (device time %s) is later than device now (%s); read the whole buffer without -T and dropped lines already stored",
		time.Unix(0, lastNs).In(loc).Format("2006-01-02 15:04:05.000"), time.Unix(0, nowNs).In(loc).Format("2006-01-02 15:04:05.000")))
}

// noteWhole records why a same-boot resume read the whole device buffer.
func (d *Device) noteWhole(msg string) {
	log.Printf("%s: %s", d.Serial, msg)
	d.rollbackSkipped.Store(0)
	d.mu.Lock()
	d.rollback = msg
	d.mu.Unlock()
}

// StartingProgress is the progress of a device whose recording has not
// started yet (Manager.WaitRecording timed out).
func StartingProgress() Progress { return Progress{Pending: true, Starting: true} }
