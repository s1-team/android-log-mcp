package device

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Recovery of lines that logd skips for the live stream while the device
// clock is set back (issue #65).
//
// logd up to Android 11 keeps a reader's position as a device time (the
// time of the last entry it sent). When it prunes a full buffer, the
// reader with the earliest position is "blocking" if the entry to prune is
// dated at or after that position, and a blocking reader that lags a few
// seconds is told to skip the next entries of that buffer (or is
// disconnected). After the device clock was set back, the oldest entries
// in the buffer are dated after the reader's position, so every prune of a
// full buffer can make the live stream skip a block of new lines. They
// stay in the device buffer for a while: a periodic dump of its newest
// entries (`logcat -d -t N` reads by count from the start of the list, not
// by time) finds them, and the holes are stored late. Android 12+ logd
// compares sequence numbers and is not affected.

// reconcileEvery is how often the device buffer is compared with the
// stored lines in repair mode.
var reconcileEvery = newDur(20 * time.Second)

// reconcileMaxSDK is the last API level whose logd positions readers by time.
const reconcileMaxSDK = 30

// pruneMargin is logd's margin around a reader's position (LogBuffer.cpp).
const pruneMargin = 3 * time.Second

// reconcileCmd dumps the newest n entries of the recorded buffers, in the
// stream's format.
func reconcileCmd(n int) string {
	return "logcat -d -t " + strconv.Itoa(n) + " -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable -D"
}

// logdTimePositions reports whether the device's logd positions readers by
// device time (API ≤ 30).
func (d *Device) logdTimePositions() bool {
	sdk, err := strconv.Atoi(strings.TrimSpace(d.Store.Meta().SDK))
	return err == nil && sdk > 0 && sdk <= reconcileMaxSDK
}

// tailFrom is the seq before the last tail stored lines of the boot
// session (tail ≤ 0: the whole session).
func tailFrom(st *store.Store, tail int) uint64 {
	from := sessionFirst(st)
	if last := st.LastSeq(); tail > 0 && last > uint64(tail) && last-uint64(tail) > from {
		from = last - uint64(tail)
	}
	return from
}

// backJumpAfter reports whether a clock jump back was recorded after seq from.
func backJumpAfter(st *store.Store, from uint64) bool {
	for _, j := range st.ClockJumps() {
		if j.Seq > from && j.DeltaNs < 0 {
			return true
		}
	}
	return false
}

// datedAfter counts the stored lines after seq from dated after ns.
func datedAfter(st *store.Store, from uint64, ns int64) int {
	n := 0
	st.Scan(from, st.LastSeq()+1, true, func(r *model.Record) bool {
		if r.TimeNs > ns {
			n++
		}
		return true
	})
	return n
}

// repairNeeded reports whether logd can make the live stream skip lines:
// the device buffer (its newest bufN entries are the last bufN stored
// lines, give or take) still holds lines dated after the stream's
// position, which is about device now. That happens only after a clock
// jump back, and ends by itself once those lines have rotated out.
func (d *Device) repairNeeded(bufN uint64) bool {
	from := tailFrom(d.Store, tailSize(bufN))
	if !backJumpAfter(d.Store, from) {
		return false
	}
	now := d.Clock().Now.UnixNano()
	return now != 0 && datedAfter(d.Store, from, now-int64(pruneMargin)) > 0
}

// reconcileLoop is the repair mode (issue #65): while repairNeeded, it
// dumps the device buffer every reconcileEvery, compares it with the
// stored lines and sends the lines the stream skipped to out. Outside it,
// it only checks the recorded clock jumps (no adb call).
func (d *Device) reconcileLoop(ctx context.Context, caught <-chan struct{}, out chan<- []*model.Record) {
	if !d.logdTimePositions() {
		return
	}
	select {
	case <-caught:
	case <-ctx.Done():
		return
	}
	defer d.setRepair(false, "")
	var prev uint64 // last stored seq at the previous comparison (0: none)
	var bufN uint64 // entries in the device buffers (logcat -S)
	var bufAt time.Time
	// lines sent but maybe not stored yet (the pipeline holds lines for
	// its Delay): not sent again
	sent := multiset{}
	var idx sessionIndex // the stored lines of the session, per repair mode
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(reconcileEvery.Get())
		if !backJumpAfter(d.Store, sessionFirst(d.Store)) {
			d.setRepair(false, "")
			prev = 0
			idx = sessionIndex{}
			continue
		}
		if bufAt.IsZero() || time.Since(bufAt) > 10*time.Minute {
			if n := d.bufferLines(ctx); n > 0 {
				bufN, bufAt = n, time.Now()
			}
		}
		seq := d.Store.LastSeq()
		final := false
		if !d.repairNeeded(bufN) {
			if prev == 0 {
				d.setRepair(false, "no line dated after device now is left in the device buffer")
				clear(sent)
				idx = sessionIndex{}
				continue
			}
			// one more read: lines skipped since the previous one
			final = true
		} else {
			d.setRepair(true, "the device buffer holds lines dated after device now (clock set back)")
		}
		n := reconcileSize(prev, seq, bufN)
		recs, err := d.dumpTail(ctx, n)
		prev = seq // a failed whole read is not retried: the next reads cover what follows
		if final {
			prev = 0
		}
		if err != nil {
			log.Printf("%s: repair read of %d entries failed: %v", d.Serial, n, err)
			continue
		}
		var missing []*model.Record
		// lines the dump printed that the stream still holds (pipeline
		// Delay) are stored before the compare
		select {
		case <-time.After(reconcileSettle.Get()):
		case <-ctx.Done():
			return
		}
		idx.update(d.Store)
		for _, r := range idx.holes(recs) {
			if sent[recHash(r)] == 0 {
				missing = append(missing, r)
			}
		}
		if len(sent) > maxResumeHashes {
			clear(sent)
		}
		for _, r := range missing {
			sent[recHash(r)]++
		}
		if len(missing) == 0 {
			continue
		}
		select {
		case out <- missing:
		case <-ctx.Done():
			return
		}
	}
}

// reconcileSettle: wait this long after a read before comparing it, so
// lines the stream delivered meanwhile are stored (pipeline Delay 800 ms).
var reconcileSettle = newDur(2 * time.Second)

// reconcileSize is how many entries to dump. A skip only hits entries
// logged after the stream's position, so a read must cover the entries
// logged since the previous read: the lines stored since then plus the
// ones skipped (logd skips at most 256 per prune), so twice the stored
// ones plus a margin, at least 2000. Never more than the buffer holds
// (bufN entries, logcat -S), and the whole buffer on the first read of a
// repair mode (a skip during the catch-up read or before the mode began).
func reconcileSize(prev, seq, bufN uint64) int {
	whole := int(bufN)
	if bufN == 0 || bufN > maxResumeHashes {
		whole = maxResumeHashes
	}
	if prev == 0 || seq < prev {
		return whole
	}
	return min(max(int(2*(seq-prev))+500, 2000), whole)
}

// dumpTail's timeout: an API 29 emulator prints about 470 entries per
// second through adb shell (a whole 2 MiB × 4 buffer took 2m40s).
func dumpTimeout(n int) time.Duration {
	return 30*time.Second + time.Duration(n)*5*time.Millisecond
}

// dumpTail reads and assembles the newest n entries of the device buffer.
func (d *Device) dumpTail(ctx context.Context, n int) ([]*model.Record, error) {
	out, err := d.adb.Shell(ctx, d.Addr(), dumpTimeout(n), reconcileCmd(n))
	if err != nil {
		return nil, err
	}
	asm := &logcat.Assembler{Loc: d.Loc(), Year: time.Now().Year()}
	var recs []*model.Record
	for _, l := range strings.Split(out, "\n") {
		if r := asm.Push(strings.TrimRight(l, "\r")); r != nil {
			recs = append(recs, r)
		}
	}
	if r := asm.Flush(); r != nil {
		recs = append(recs, r)
	}
	return recs, nil
}

// headerHash identifies a record by device time (µs), pid and tid only: a
// stored record split or merged differently from the dump, or printed
// with another tag (logd renders some audit entries differently in a
// dump), still counts as stored.
func headerHash(r *model.Record) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d", r.TimeNs, r.PID, r.TID)
	return h.Sum64()
}

// sessionIndex counts the stored lines of the boot session (newest
// maxResumeHashes at most) by record hash and by header hash, per copy. A
// repair read can hold any stored line of the session, in any order
// relative to the store (one buffer can keep boot lines while another
// rotated many times), so it is compared with all of them (issue #75). The
// index is built once per repair mode and extended with new seqs, so a
// read does not rescan the store.
type sessionIndex struct {
	first      uint64 // sessionFirst when built
	next       uint64 // the next seq to add
	recs, hdrs multiset
	complete   bool // holds the session's first line
}

func (x *sessionIndex) add(r *model.Record) {
	x.recs[recHash(r)]++
	x.hdrs[headerHash(r)]++
}

// update brings the index up to the store's newest line.
func (x *sessionIndex) update(st *store.Store) {
	sf := sessionFirst(st)
	last := st.LastSeq()
	if x.recs == nil || x.first != sf || len(x.recs) > 2*maxResumeHashes {
		*x = sessionIndex{first: sf, next: last + 1, recs: multiset{}, hdrs: multiset{}}
		seen := 0
		st.Scan(sf, last+1, true, func(r *model.Record) bool {
			if r.Seq == sf+1 {
				x.complete = true
			}
			x.add(r)
			seen++
			return seen < maxResumeHashes
		})
		return
	}
	if last < x.next {
		return
	}
	st.Scan(x.next-1, last+1, false, func(r *model.Record) bool {
		x.add(r)
		return true
	})
	x.next = last + 1
}

// holes returns the dumped records that are not stored although records
// before and after them in buffer order are: the lines the stream
// skipped. Each stored line of the session matches one dumped copy
// (identical lines count per copy); a stored line with the same header but
// split or merged differently matches too. Records after the last stored
// one (not streamed yet) are never returned, nor records before the first
// stored one unless the index holds the session's first line (the session
// began with the whole device buffer, so a dumped line before the first
// stored one was skipped too: logd kicked the catch-up read).
func (x *sessionIndex) holes(recs []*model.Record) []*model.Record {
	used, usedH := multiset{}, multiset{}
	present := make([]bool, len(recs))
	first, last := -1, -1
	for i, r := range recs {
		h, hh := recHash(r), headerHash(r)
		ok := used[h] < x.recs[h]
		if ok {
			used[h]++
		}
		if usedH[hh] < x.hdrs[hh] {
			usedH[hh]++
			ok = true
		}
		if ok {
			present[i] = true
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if x.complete {
		first = -1
	}
	var out []*model.Record
	for i := first + 1; i < last; i++ {
		if !present[i] {
			out = append(out, recs[i])
		}
	}
	return out
}

// holes compares recs with all stored lines of the session (tests).
func holes(recs []*model.Record, st *store.Store) []*model.Record {
	x := &sessionIndex{}
	x.update(st)
	return x.holes(recs)
}

// setRepair switches the repair mode, logging the change (service log)
// and keeping it for logcat_status.
func (d *Device) setRepair(on bool, why string) {
	d.mu.Lock()
	was := d.repairSince != 0
	if on == was {
		d.mu.Unlock()
		return
	}
	if on {
		d.repairSince = time.Now().UnixNano()
	} else {
		d.repairSince = 0
	}
	d.mu.Unlock()
	if on {
		log.Printf("%s: repair reads on (logd of Android 11 and older can skip lines of the live stream): %s", d.Serial, why)
	} else if why != "" {
		log.Printf("%s: repair reads off: %s; recovered %d lines so far", d.Serial, why, d.recovered.Load())
	}
}

// storeRecovered queues lines found by reconcileLoop (stream goroutine).
func (d *Device) storeRecovered(pipe *Pipeline, recs []*model.Record) {
	now := time.Now()
	for _, r := range recs {
		pipe.AcceptLate(r, now)
	}
	d.recovered.Add(int64(len(recs)))
	log.Printf("%s: recovered %d lines that logd skipped for the live stream while the device clock was set back", d.Serial, len(recs))
}

// RecoveredNote describes the repair mode and the lines it recovered
// (empty if neither).
func (d *Device) RecoveredNote() string {
	n := d.recovered.Load()
	d.mu.RLock()
	since := d.repairSince
	d.mu.RUnlock()
	s := ""
	if since != 0 {
		s = "repair reads on since host time " + time.Unix(0, since).Format("15:04:05") + ": the device clock was set back and logd (Android 11 and older) can skip lines of the live stream while the device buffer holds lines dated after device now; the buffer is compared with the stored lines every " + reconcileEvery.Get().String()
	}
	if n > 0 {
		if s != "" {
			s += "; "
		}
		s += fmt.Sprintf("recovered %d lines that logd skipped (stored in the order they were recovered)", n)
	}
	return s
}
