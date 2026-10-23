package store

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/clock"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// DefaultResumeRingCap is the fixed ring capacity N: last N arrivals of the
// current boot. Same bound as MaxResumeDedup / the device maxResumeHashes
// (500k). Issue #259: fixed N day one (not measured from logcat -S).
const DefaultResumeRingCap = MaxResumeDedup

// RecordHash is the resume/dedup content hash for one stored or replayed
// line (FNV-64a over TimeNs|PID|TID|Level|Tag|Msg). Buffer and UID are
// intentionally excluded (#259 Q6). Must stay identical to device.recHash.
func RecordHash(r *model.Record) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d|%c|%s|%s", r.TimeNs, r.PID, r.TID, r.Level, r.Tag, r.Msg)
	return h.Sum64()
}

// ResumeRing is a rolling multiset of content hashes for the last N
// arrivals of one boot, ordered by ArrivalSeq. Rebuild-on-open is equivalent
// to today's device.tailMultiset + splitRuns (#192). Device maintains the
// ring on append and uses it as the unified resume compare set (#259 Phase
// B); set-root exports via Snapshot (#259 Phase C). Day-one persistence is
// memory + rebuild only (no warm-persist without set-root).
type ResumeRing struct {
	boot   string
	n      int
	order  []uint64 // circular buffer of arrival hashes, ascending ArrivalSeq
	head   int      // index of the oldest arrival when length == n
	length int      // filled arrival slots (≤ n)
	counts map[uint64]int32
	// open multi-line run for ObserveRecord (ascending arrival order)
	runHead string
	runMsgs []string
	runR0   model.Record
}

// NewResumeRing returns an empty ring for boot with capacity n. n ≤ 0 or
// n > DefaultResumeRingCap uses DefaultResumeRingCap.
func NewResumeRing(boot string, n int) *ResumeRing {
	if n <= 0 || n > DefaultResumeRingCap {
		n = DefaultResumeRingCap
	}
	return &ResumeRing{
		boot:   boot,
		n:      n,
		order:  make([]uint64, n),
		counts: make(map[uint64]int32),
	}
}

// Reset clears the ring for a (possibly new) boot.
func (r *ResumeRing) Reset(boot string) {
	r.boot = boot
	r.head = 0
	r.length = 0
	clear(r.counts)
	r.runHead = ""
	r.runMsgs = r.runMsgs[:0]
}

// Boot is the full boot id the ring is keyed to.
func (r *ResumeRing) Boot() string { return r.boot }

// Cap is the arrival capacity N.
func (r *ResumeRing) Cap() int { return r.n }

// Len is the number of arrival slots filled (not the multiset key count;
// splitRuns whole-entry hashes add to the multiset without a slot).
func (r *ResumeRing) Len() int { return r.length }

// Observe appends one arrival's content hash. seq is ArrivalSeq (#258
// soft-alias); membership is seq-ordered within a boot, never EventClock
// Continuous/DeviceInstant. When over capacity, the oldest arrival hash
// is dropped from the multiset.
func (r *ResumeRing) Observe(seq clock.ArrivalSeq, h uint64) {
	_ = seq // order is append order; seq documents the ArrivalSeq seam
	if r.length == r.n {
		old := r.order[r.head]
		r.order[r.head] = h
		r.head = (r.head + 1) % r.n
		if r.counts[old] <= 1 {
			delete(r.counts, old)
		} else {
			r.counts[old]--
		}
	} else {
		r.order[(r.head+r.length)%r.n] = h
		r.length++
	}
	r.counts[h]++
}

// Take consumes one multiset count of h (resume replay drop). False if
// none is left. Does not change the arrival deque — same contract as the
// device multiset used during catch-up.
func (r *ResumeRing) Take(h uint64) bool {
	if r.counts[h] > 0 {
		r.counts[h]--
		if r.counts[h] == 0 {
			delete(r.counts, h)
		}
		return true
	}
	return false
}

// Count returns the remaining multiset count of h.
func (r *ResumeRing) Count(h uint64) int32 { return r.counts[h] }

// Counts returns a copy of the multiset with positive counts only.
func (r *ResumeRing) Counts() map[uint64]int32 {
	out := make(map[uint64]int32, len(r.counts))
	for h, n := range r.counts {
		if n > 0 {
			out[h] = n
		}
	}
	return out
}

// Snapshot expands the multiset to a slice for set-root carry
// (resume_dedup.bin / ContinueIn).
func (r *ResumeRing) Snapshot() []uint64 {
	var out []uint64
	for h, n := range r.counts {
		for ; n > 0; n-- {
			out = append(out, h)
		}
	}
	return out
}

// MergeExport adds set-root carried hashes into the multiset without
// touching the ArrivalSeq deque. Callers that keep a live ring across
// resumes should merge into a per-resume counts copy (Active can span
// many resumes) rather than the live ring itself.
func (r *ResumeRing) MergeExport(hashes []uint64) {
	for _, h := range hashes {
		r.counts[h]++
	}
}

// ObserveRecord hashes the record, observes it at rec.Seq, and tracks
// multi-line runs so a whole-entry hash is counted when a run of
// same-header pieces ends (#192). Ascending arrival order — the live
// append path. Call FlushRun when the session ends if the last run may
// still be open.
func (r *ResumeRing) ObserveRecord(rec *model.Record) {
	if rec == nil {
		return
	}
	r.Observe(clock.ArrivalSeq(rec.Seq), RecordHash(rec))
	rh := recordRunHeader(rec)
	if len(r.runMsgs) > 0 && rh != r.runHead {
		r.endRunAscending()
	}
	if len(r.runMsgs) == 0 {
		r.runHead, r.runR0 = rh, *rec
	}
	r.runMsgs = append(r.runMsgs, rec.Msg)
}

// FlushRun closes any open multi-line run (Phase B end-of-batch).
func (r *ResumeRing) FlushRun() { r.endRunAscending() }

func (r *ResumeRing) endRunAscending() {
	if n := len(r.runMsgs); n > 1 {
		whole := r.runR0
		whole.Msg = strings.Join(r.runMsgs, "\n")
		// Whole-entry extras live only in the multiset (not the deque),
		// matching today's tailMultiset. Phase B maintain-on-append must
		// keep rebuild-on-open as the cold path so extras stay in window.
		r.counts[RecordHash(&whole)]++
	}
	r.runMsgs = r.runMsgs[:0]
	r.runHead = ""
}

func recordRunHeader(r *model.Record) string {
	return fmt.Sprintf("%d|%d|%d|%c|%s|%d|%s", r.TimeNs, r.PID, r.TID, r.Level, r.Tag, r.UID, r.Buffer)
}

// sessionFirstSeq is the seq before the current boot session's first line
// (same bound device.sessionFirst uses for tailMultiset).
func sessionFirstSeq(st *Store) uint64 {
	m := st.Meta()
	if len(m.Sessions) == 0 {
		return 0
	}
	if f := m.Sessions[len(m.Sessions)-1].FirstSeq; f > 0 {
		return f - 1
	}
	return 0
}

// rebuildSplitRuns mirrors device.splitRuns for a newest-first scan: for
// every run of consecutive stored records with one header, also count the
// hash the whole entry would have (messages joined by "\n").
type rebuildSplitRuns struct {
	head string
	msgs []string
	r0   model.Record
}

func (s *rebuildSplitRuns) add(set map[uint64]int32, r *model.Record) {
	h := recordRunHeader(r)
	if len(s.msgs) > 0 && h != s.head {
		s.end(set)
	}
	if len(s.msgs) == 0 {
		s.head, s.r0 = h, *r
	}
	s.msgs = append(s.msgs, r.Msg)
}

func (s *rebuildSplitRuns) end(set map[uint64]int32) {
	if n := len(s.msgs); n > 1 {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			s.msgs[i], s.msgs[j] = s.msgs[j], s.msgs[i]
		}
		whole := s.r0
		whole.Msg = strings.Join(s.msgs, "\n")
		set[RecordHash(&whole)]++
	}
	s.msgs = s.msgs[:0]
}

// RebuildResumeRing scans the last n arrivals of the current boot by
// ArrivalSeq and returns a ring whose multiset equals today's
// device.tailMultiset(st, n), including splitRuns whole-entry hashes
// (#192 / #259 Phase A). n ≤ 0 or oversized uses DefaultResumeRingCap.
func RebuildResumeRing(st *Store, n int) *ResumeRing {
	boot := ""
	if st != nil {
		boot = st.Meta().LastBootID
	}
	ring := NewResumeRing(boot, n)
	if st == nil {
		return ring
	}
	n = ring.n
	type item struct {
		seq, h uint64
	}
	items := make([]item, 0, 256)
	var runs rebuildSplitRuns
	seen := 0
	st.Scan(sessionFirstSeq(st), st.LastSeq()+1, true, func(rec *model.Record) bool {
		h := RecordHash(rec)
		items = append(items, item{seq: rec.Seq, h: h})
		ring.counts[h]++
		runs.add(ring.counts, rec)
		seen++
		return seen < n
	})
	runs.end(ring.counts)
	// items are newest-first; fill the deque ascending without re-counting
	for i := len(items) - 1; i >= 0; i-- {
		ring.order[ring.length] = items[i].h
		ring.length++
	}
	return ring
}
