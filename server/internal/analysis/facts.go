package analysis

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// Persisted-fact support for issue #225 (design revision 2, PR1): evidence
// on a crash/ANR, its identity and content key, the pure merge of an
// incoming snapshot into the latest stored version, and the lookup of an
// open fact among the stored ones. The fields are additive and optional in
// crashes.jsonl: readers ignore unknown keys, no format version changes.

// Evidence is one observation attached to a fact (crashes.jsonl "ev").
type Evidence struct {
	S  string `json:"s"`            // source, e.g. events:am_anr, logcat:system, dropbox:data_app_anr
	Q  uint64 `json:"q,omitempty"`  // seq of the first record (0: none, e.g. DropBox)
	T  int64  `json:"t,omitempty"`  // device time (ns)
	CK string `json:"ck,omitempty"` // content key (DropBox: the body key)
	HT int64  `json:"ht,omitempty"` // host time when attached (ns)
}

// Evidence classes.
const (
	ClassEvents  = 1 << iota // events:*
	ClassLogcat              // logcat:*
	ClassDropbox             // dropbox:*
)

// SourceClass is the evidence class of a source string (0: unknown).
func SourceClass(s string) int {
	switch {
	case strings.HasPrefix(s, "events:"):
		return ClassEvents
	case strings.HasPrefix(s, "logcat:"):
		return ClassLogcat
	case strings.HasPrefix(s, "dropbox:"):
		return ClassDropbox
	}
	return 0
}

// Same reports that two evidences are the same observation (idempotency):
// the same class and seq when both carry a seq, else the same class, time
// and content key. An evidence without seq and content key (synthesized
// from a legacy line) has no identity. ck alone never makes a repeat.
func (e Evidence) Same(o Evidence) bool {
	if SourceClass(e.S) != SourceClass(o.S) || SourceClass(e.S) == 0 {
		return false
	}
	if e.Q != 0 || o.Q != 0 {
		return e.Q == o.Q
	}
	return e.CK != "" && e.CK == o.CK && e.T == o.T
}

// HasEvidence reports that the crash was written by code that records
// evidence; a stored line without "ev" is a legacy fact (0.9.x and older,
// or rewritten by an old binary).
func (c *Crash) HasEvidence() bool { return len(c.Ev) > 0 }

// Classes is the set of evidence classes the crash holds, from "ev" or, for
// a legacy fact, from "sources".
func (c *Crash) Classes() int {
	n := 0
	for _, e := range c.Ev {
		n |= SourceClass(e.S)
	}
	for _, s := range c.Sources {
		n |= SourceClass(s)
	}
	return n
}

// AddEvidence appends e unless the crash already holds the same evidence;
// it reports whether it was added. The source is added too.
func (c *Crash) AddEvidence(e Evidence) bool {
	for _, x := range c.Ev {
		if x.Same(e) || x.S == e.S && x.Q == e.Q && x.T == e.T && x.CK == e.CK {
			return false
		}
	}
	c.Ev = append(c.Ev, e)
	c.addSource(e.S)
	return true
}

// HasEvidenceOf reports that some evidence of o is already on c.
func (c *Crash) HasEvidenceOf(o *Crash) bool {
	for _, e := range o.Ev {
		for _, x := range c.Ev {
			if x.Same(e) {
				return true
			}
		}
	}
	return false
}

// synthEvidence gives every source without evidence an identity-less entry
// (it marks the class as present for one-to-one pairing).
func (c *Crash) synthEvidence() {
	for _, s := range c.Sources {
		has := false
		for _, e := range c.Ev {
			if e.S == s {
				has = true
				break
			}
		}
		if !has {
			c.Ev = append(c.Ev, Evidence{S: s})
		}
	}
}

// ---- content key (design 3.1.2) ----

var ckRes = []*regexp.Regexp{
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?`),
	regexp.MustCompile(`\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?`),
	regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(\.\d+)?\b`),
	regexp.MustCompile(`\b\d{10,13}(\.\d+)?\b`),
	regexp.MustCompile(`\b\d+(\.\d+)?\s?(ms|s|sec|seconds)\b`),
	// the parenthesised form first: after the bare "pid N" rule it could
	// never match (the issue lists them in the other order, which leaves
	// "(pid N)" as "(#)" and makes that rule dead)
	regexp.MustCompile(`\(pid \d+\)`),
	regexp.MustCompile(`(?i)\b(pid|tid)[ :=]+\d+`),
	regexp.MustCompile(`0x[0-9a-fA-F]+`),
	regexp.MustCompile(`@[0-9a-f]{6,}`),
}

// NormalizeReason is the text input of a content key: whitespace collapsed,
// timestamps, epoch numbers, durations, pids/tids and hex values replaced by
// "#", cut to 512 bytes.
func NormalizeReason(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Join(strings.Fields(s), " ")
	for _, re := range ckRes {
		s = re.ReplaceAllString(s, "#")
	}
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// ContentKey is the content key of an evidence of the given class (a
// SourceClass value's source prefix: "events", "logcat"): FNV-64a, 16 hex,
// of class NUL process NUL normalized reason. It is only ever compared
// inside one fact.
func ContentKey(class, process, reason string) string {
	h := fnv.New64a()
	h.Write([]byte(class + "\x00" + process + "\x00" + NormalizeReason(reason)))
	return fmt.Sprintf("%016x", h.Sum64())
}

// ---- merge (design 3.2.1) ----

// MergeMode says how an incoming snapshot relates to the stored fact.
type MergeMode struct {
	// Looked: the fact was found by lookup (restart or later source), not
	// carried by the snapshot's own ID: stored sources come first.
	Looked bool
	// NoDropboxFill: do not fill the DropBox text from the stored version
	// (the caller already merged it, as AdoptDropbox does).
	NoDropboxFill bool
}

// MergeFact merges the incoming snapshot in into the latest stored version
// of the same fact and returns the version to append. It is pure: neither
// argument is modified. Evidence is the union by identity, sources are
// re-derived from it, DropBox-derived and optional text fields are filled
// when empty (this generalizes KeepDropbox), and a snapshot that only holds
// events never replaces the text of a logcat or tombstone version.
func MergeFact(latest, in *Crash, m MergeMode) *Crash {
	out := *in
	out.Sources = append([]string(nil), in.Sources...)
	out.Ev = append([]Evidence(nil), in.Ev...)
	if latest == nil {
		out.synthEvidence()
		return &out
	}
	lat := *latest
	lat.synthEvidence()
	inLogcat := in.Classes()&ClassLogcat != 0
	preferLatest := latest.Kind == "anr" || !inLogcat

	out.ID = latest.ID
	if latest.Kind != "" {
		out.Kind = latest.Kind
	}
	if latest.Boot != "" {
		out.Boot = latest.Boot
	}
	// anchor (first evidence): an ANR keeps the stored one; Java and native
	// take the logcat record's, which upgrades an events-only version
	switch {
	case latest.Kind != "anr" && inLogcat && in.TimeNs != 0 && latest.Classes()&ClassLogcat == 0:
	case latest.Seq != 0 || in.Seq == 0 && latest.TimeNs != 0:
		out.TimeNs, out.Seq = latest.TimeNs, latest.Seq
	}
	if out.PID == 0 {
		out.PID = latest.PID
	}
	pick := func(inV, latV string, prefer bool) string {
		switch {
		case inV == "":
			return latV
		case prefer && latV != "":
			return latV
		}
		return inV
	}
	out.Process = pick(in.Process, latest.Process, preferLatest)
	out.Package = pick(in.Package, latest.Package, preferLatest)
	out.Headline = pick(in.Headline, latest.Headline, preferLatest)
	textLatest := !inLogcat
	out.Thread = pick(in.Thread, latest.Thread, textLatest)
	out.Body = pick(in.Body, latest.Body, textLatest)
	out.Signature = pick(in.Signature, latest.Signature, textLatest)
	fill := func(v *string, l string) {
		if *v == "" {
			*v = l
		}
	}
	fill(&out.Traces, latest.Traces)
	fill(&out.CPU, latest.CPU)
	fill(&out.Tombstone, latest.Tombstone)
	if !m.NoDropboxFill {
		fill(&out.Dropbox, latest.Dropbox)
	}
	fill(&out.Location, latest.Location)

	// evidence: union by identity, stored first
	var ev []Evidence
	ev = append(ev, lat.Ev...)
	for _, e := range in.Ev {
		dup := false
		for _, x := range ev {
			if x.Same(e) || x.S == e.S && x.Q == e.Q && x.T == e.T && x.CK == e.CK {
				dup = true
				break
			}
		}
		if !dup {
			ev = append(ev, e)
		}
	}
	// an identity-less synthesized entry is dropped once the real one of
	// that source is there
	kept := ev[:0:0]
	for i, e := range ev {
		if e.Q == 0 && e.T == 0 && e.CK == "" {
			real := false
			for j, x := range ev {
				if j != i && x.S == e.S && (x.Q != 0 || x.CK != "") {
					real = true
				}
			}
			if real {
				continue
			}
		}
		kept = append(kept, e)
	}
	out.Ev = kept
	// sources
	var src []string
	add := func(l []string) {
		for _, s := range l {
			dup := false
			for _, x := range src {
				if x == s {
					dup = true
					break
				}
			}
			if !dup {
				src = append(src, s)
			}
		}
	}
	if m.Looked {
		add(latest.Sources)
		add(in.Sources)
	} else {
		add(in.Sources)
		add(latest.Sources)
	}
	for _, e := range out.Ev {
		add([]string{e.S})
	}
	out.Sources = src
	// incoming binding fields are decided by the caller (design 3.1.1)
	out.Inc, out.IncWeak = latest.Inc, latest.IncWeak
	out.N = latest.N // the ordinal is fixed at creation
	return &out
}

// ProcCompatible reports that two process names may be the same process:
// an empty name does not contradict, equal names, equal packages and the
// 15-character thread-name form of a long name are compatible.
func ProcCompatible(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	return pkgOf(a) == pkgOf(b) || shortProc(a) == shortProc(b)
}

// Windows of the pairing of a detector source with a stored fact. PR1 of
// issue #225 keeps the windows the detector used; later PRs replace the
// ones for facts that carry evidence. A fact without "ev" (legacy) is
// always paired inside these.
const (
	legacyAnrWindow   = anrPairWindow
	legacyCrashWindow = int64(60e9)
	javaMergeWindow   = int64(15e9)
)

// FindOpenFact looks for the stored fact that the detector snapshot in
// (ID == "") belongs to: cands are the stored facts of in's boot and kind
// (and pid), newest first. repeat reports that the fact already holds one of
// in's evidences (a replay: nothing to attach). compat is the incarnation
// check of the caller (nil: all). DropBox-only facts are left to
// AdoptDropbox.
func FindOpenFact(cands []*Crash, in *Crash, jumps []ClockShift, compat func(*Crash) bool) (f *Crash, repeat bool) {
	ok := func(x *Crash) bool {
		if x.Kind != in.Kind || x.ID == "" {
			return false
		}
		if x.Boot != "" && in.Boot != "" && x.Boot != in.Boot {
			return false
		}
		switch in.Kind {
		case "native":
			if x.PID != 0 && in.PID != 0 && x.PID != in.PID {
				return false
			}
			return sameProc(x.Process, in.Process)
		default:
			return x.PID == in.PID
		}
	}
	for _, x := range cands {
		if ok(x) && x.HasEvidenceOf(in) {
			return x, true
		}
	}
	if in.Kind == "anr" {
		// ordinal / interval pairing (anrfact.go); legacy facts keep the window
		return PickAnrFact(cands, in, func(x *Crash) int64 { return signedDelta(x.TimeNs, x.Seq, in.TimeNs, jumps) }, compat)
	}
	for _, x := range cands {
		if !ok(x) || dropboxOnly(x) {
			continue
		}
		win := legacyCrashWindow
		switch in.Kind {
		case "java":
			win = javaMergeWindow
		case "native":
			// one fact per source of a native crash: the libc line
			// (logcat:crash), the tombstone lines (logcat:tombstone) and
			// am_crash are three different sources of one crash (issue
			// #233), a second one of the same source is another crash
			if x.hasAnySource(in) {
				continue
			}
		}
		if d, _ := dropboxDelta(x.TimeNs, x.Seq, in.TimeNs, jumps); d > win {
			continue
		}
		if compat != nil && !compat(x) {
			continue
		}
		return x, false
	}
	return nil, false
}

// hasAnySource reports that c already holds a source string of in's
// evidence.
func (c *Crash) hasAnySource(in *Crash) bool {
	for _, e := range in.Ev {
		for _, s := range c.Sources {
			if s == e.S {
				return true
			}
		}
	}
	return false
}
