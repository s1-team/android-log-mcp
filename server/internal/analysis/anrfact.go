package analysis

import (
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/clock"
)

// ANR pairing by ordinal and interval (issue #225, design revision 2, PR2).
//
// The sources of one ANR (am_anr event, "ANR in" block, DropBox entry) are
// not paired by a time window any more but by the order they arrived in:
// evidence attaches to the latest ANR of its process lifetime that was
// anchored before it, if that ANR still lacks the source and the content
// agrees. Time is only a veto cap for a source that was lost for good.
// Facts written by 0.9.x (no "ev") keep the old window, never the ordinal.

// anrEvidenceCap vetoes a pairing that order and content allow but that is
// implausible: evidence later than this after the anchor of the ANR (on the
// jump-normalized axis) is a new ANR, and a DropBox entry that late stays
// unmatched. It is not tuned to device load: a slow dump (30-100 s on a
// TCG emulator) is accepted up to the cap and not beyond.
const anrEvidenceCap = int64(10 * 60 * 1e9)

// ReasonCompatible compares the reason of two sources of an ANR after the
// content-key normalization: equal, one a prefix of the other, or equal up
// to the first " (" / " {" are compatible. known is false when either side
// has no reason: then the text neither vetoes nor confirms.
func ReasonCompatible(a, b string) (ok, known bool) {
	a, b = NormalizeReason(a), NormalizeReason(b)
	if a == "" || b == "" {
		return true, false
	}
	if a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
		return true, true
	}
	cut := func(s string) string {
		for _, sep := range []string{" (", " {"} {
			if i := strings.Index(s, sep); i >= 0 {
				s = s[:i]
			}
		}
		return s
	}
	return cut(a) == cut(b), true
}

// signedDelta is the evidence time minus the anchor time on the jump
// normalized axis: EventClock.ShiftSigned (same min-abs choice as dropboxDelta).
func signedDelta(anchorNs int64, anchorSeq uint64, evNs int64, jumps []ClockShift) int64 {
	ec := eventClockOf(jumps)
	return ec.ShiftSigned(ec.Stamp(clock.ArrivalSeq(anchorSeq), clock.DeviceInstant(anchorNs)), clock.DeviceInstant(evNs))
}

// anchoredBefore is the interval rule's order test: the anchor of x is not
// after the evidence. By arrival (seq) when both have one, else by time
// with the DropBox slack.
func anchoredBefore(x *Crash, evSeq uint64, delta int64) bool {
	if x.Seq != 0 && evSeq != 0 {
		return x.Seq <= evSeq
	}
	return delta >= -anrEntrySlack
}

// PickAnrFact chooses the ANR among cands that the evidence of in (one
// source class; in.Seq, in.TimeNs, in.Headline = its reason) attaches to.
// cands are ANR facts of in's boot and pid. delta(x) is the signed distance
// from the anchor of x to in on the jump-normalized axis, same(x) the
// incarnation check (nil: all). repeat reports that a fact already holds
// the same evidence. A fact without "ev" (legacy) is paired only inside
// legacyAnrWindow and one-to-one; a fact with "ev" by the interval rule:
// the latest of the lifetime anchored before the evidence, open for the
// class, content-compatible, inside anrEvidenceCap. A missing reason is
// not a veto, but then the target must be the only open candidate.
func PickAnrFact(cands []*Crash, in *Crash, delta func(x *Crash) int64, same func(x *Crash) bool) (f *Crash, repeat bool) {
	inCl := in.Classes()
	var fresh, legacy []*Crash
	for _, x := range cands {
		if x.Kind != "anr" || x.PID != in.PID && x.PID != 0 && in.PID != 0 {
			continue
		}
		if x.Boot != "" && in.Boot != "" && x.Boot != in.Boot {
			continue
		}
		if x.HasEvidenceOf(in) {
			return x, true
		}
		if dropboxOnly(x) || same != nil && !same(x) {
			continue
		}
		if x.HasEvidence() && !x.synthOnly() {
			fresh = append(fresh, x)
		} else {
			legacy = append(legacy, x)
		}
	}
	// ordinal / interval rule
	var latest *Crash
	for _, x := range fresh {
		if !anchoredBefore(x, in.Seq, delta(x)) {
			continue
		}
		if latest == nil || x.Seq > latest.Seq || x.Seq == latest.Seq && delta(x) < delta(latest) {
			latest = x
		}
	}
	if latest != nil {
		open := func(x *Crash) (ok, unknownReason bool) {
			if x.Classes()&inCl != 0 || abs(delta(x)) > anrEvidenceCap {
				return false, false
			}
			rok, known := ReasonCompatible(x.Headline, in.Headline)
			return rok, !known
		}
		if ok, unknown := open(latest); ok {
			if unknown {
				for _, x := range fresh {
					if x != latest && anchoredBefore(x, in.Seq, delta(x)) {
						if o, _ := open(x); o {
							return nil, false // two open candidates, no reason to tell: unmatched
						}
					}
				}
			}
			return latest, false
		}
		return nil, false // the latest ANR of the lifetime cannot take it: a new ANR
	}
	for _, x := range legacy {
		if x.Classes()&inCl != 0 {
			continue // one-to-one
		}
		if d := delta(x); abs(d) > legacyAnrWindow {
			continue
		}
		return x, false
	}
	return nil, false
}

// synthOnly reports that every evidence of the fact is an identity-less
// entry synthesized from a legacy line (it names a class but was recorded
// by nothing): such a fact is a legacy fact for pairing.
func (c *Crash) synthOnly() bool {
	for _, e := range c.Ev {
		if e.Q != 0 || e.T != 0 || e.CK != "" {
			return false
		}
	}
	return true
}
