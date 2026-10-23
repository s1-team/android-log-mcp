package analysis

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/clock"
)

// DropboxEntry is one entry of `dumpsys dropbox --print <tag>`.
type DropboxEntry struct {
	Tag    string `json:"tag"`
	TimeNs int64  `json:"t"` // device-local wall clock, second precision
	// BodyTimeNs is the real time of an ANR entry read from its body
	// ("----- pid N at <time> -----", device zone); 0 when absent or not
	// parseable. DropBox re-dates entries after a back-jump, the body keeps
	// the time the ANR was dumped (issue #152). Not stored.
	BodyTimeNs int64  `json:"-"`
	Body       string `json:"body"`
	Note       string `json:"note,omitempty"` // e.g. "contents lost"
}

var reDropHdr = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) (\S+)(?: \((.*)\))?$`)

// MaxDropboxBody caps stored entry bodies (ANR traces can be large).
const MaxDropboxBody = 256 << 10

// ParseDropbox parses `dumpsys dropbox --print` output. Times are
// interpreted in loc (the device zone).
func ParseDropbox(out string, loc *time.Location) []DropboxEntry {
	if loc == nil {
		loc = time.Local
	}
	out = strings.ReplaceAll(out, "\r\n", "\n")
	parts := strings.Split(out, "\n========================================\n")
	var res []DropboxEntry
	for i, p := range parts {
		if i == 0 {
			// preamble; with a single entry the separator may start the output
			if j := strings.Index(p, "========================================\n"); j >= 0 {
				p = p[j+len("========================================\n"):]
			} else {
				continue
			}
		}
		nl := strings.IndexByte(p, '\n')
		hdr, body := p, ""
		if nl >= 0 {
			hdr, body = p[:nl], p[nl+1:]
		}
		m := reDropHdr.FindStringSubmatch(strings.TrimSpace(hdr))
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05.999", m[1], loc)
		if err != nil {
			continue
		}
		e := DropboxEntry{Tag: m[2], TimeNs: t.UnixNano(), Body: strings.TrimRight(body, "\n")}
		if strings.HasSuffix(e.Tag, "_anr") {
			e.BodyTimeNs = anrBodyTime(e.Body, loc)
		}
		if strings.Contains(m[3], "lost") || strings.Contains(hdr, "(no file)") {
			e.Note = "contents lost"
		}
		if len(e.Body) > MaxDropboxBody {
			e.Body = e.Body[:MaxDropboxBody] + "\n…[truncated by android-log-mcp]"
		}
		res = append(res, e)
	}
	return res
}

var reTraceHdr = regexp.MustCompile(`(?m)^----- pid (\d+) at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) -----\s*$`)

// anrBodyTime is the time of the ANR's own trace header, the one for the
// entry's PID ("----- pid N at <time> -----"; the other processes' dumps
// follow it), in loc, the device zone. 0 when there is none, it names no
// PID of this entry, or the time does not parse: the caller then keeps the
// entry's header time.
func anrBodyTime(body string, loc *time.Location) int64 {
	pid := headerField(body, "PID")
	if pid == "" {
		return 0
	}
	for _, m := range reTraceHdr.FindAllStringSubmatch(body, -1) {
		if m[1] != pid {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05.999", m[2], loc)
		if err != nil {
			return 0
		}
		return t.UnixNano()
	}
	return 0
}

// RealTimeNs is the time an entry stands for: the body time when the body
// carries one, else the DropBox header time.
func (e DropboxEntry) RealTimeNs() int64 {
	if e.BodyTimeNs != 0 {
		return e.BodyTimeNs
	}
	return e.TimeNs
}

func headerField(body, key string) string {
	for _, ln := range strings.SplitN(body, "\n\n", 2)[0:1] {
		for _, l := range strings.Split(ln, "\n") {
			if strings.HasPrefix(l, key+": ") {
				return strings.TrimSpace(strings.TrimPrefix(l, key+": "))
			}
		}
	}
	return ""
}

// MatchDropbox attaches e to an existing crash in list, or builds a new
// crash (returned with isNew=true) when nothing matches. Returns nil if the
// entry is not a crash/ANR tag or is useless.
func MatchDropbox(list []*Crash, e DropboxEntry) (c *Crash, isNew bool) {
	return MatchDropboxBoot(list, e, "", nil)
}

// ClockShift is a stored device clock jump as the DropBox matching needs
// it: Seq is the first stored line after the jump, DeltaNs its size
// (+ forward, − back). Callers pass only real jumps, never "possible" ones
// (a quiet hour carries no reliable delta), and only those of the boot
// session being matched. Converted to EventClock via eventClockOf (R1 #258).
type ClockShift struct {
	Seq     uint64
	DeltaNs int64
}

// dropboxDelta is the distance between a crash (device time crashNs, line
// crashSeq) and a DropBox entry (entryNs): EventClock.ShiftCompare (issue #93/#152).
func dropboxDelta(crashNs int64, crashSeq uint64, entryNs int64, jumps []ClockShift) (d int64, shifted bool) {
	ec := eventClockOf(jumps)
	return ec.ShiftCompare(ec.Stamp(clock.ArrivalSeq(crashSeq), clock.DeviceInstant(crashNs)), clock.DeviceInstant(entryNs))
}

// anrEntrySlack is how far after a DropBox entry's header time an ANR may
// be logged and still be its source (clock readings of the two sources
// differ a little).
const anrEntrySlack = int64(5e9)

// hasDropboxSource reports that a DropBox entry is already attached.
func (c *Crash) hasDropboxSource() bool {
	for _, x := range c.Sources {
		if strings.HasPrefix(x, "dropbox:") {
			return true
		}
	}
	return false
}

// MatchDropboxBoot is MatchDropbox for an entry fetched during boot
// session boot: a Java or native crash entry also matches a crash of that
// boot with the same pid and process whatever their device times differ
// by. A process instance crashes once, and DropBox re-times entries
// dated after device now when the clock was set back (issue #63). ANRs
// keep the time rule: a process can survive an ANR and ANR again.
//
// For an ANR the time of the entry is the one in its body when it has one
// (issue #152), else the header time; the header time stays a candidate.
//
// Kind, pid, process and boot must agree; the window (legacy facts: 60 s, ANR 120 s; else
// anrEvidenceCap) is checked against the raw time difference and against
// the one shifted by the clock jumps stored after the crash (jumps, issue
// #93). When two different crashes are equally near and one of them got
// there through a shift, the entry stays unmatched: a missing attachment
// beats a wrong merge.
func MatchDropboxBoot(list []*Crash, e DropboxEntry, boot string, jumps []ClockShift) (c *Crash, isNew bool) {
	return MatchDropboxIn(list, e, boot, jumps, nil)
}

// MatchDropboxIn is MatchDropboxBoot with the incarnation veto of issue
// #225 (design 3.1.1): same(x, pid, ns) reports whether the stored fact x
// and the process lifetime that the entry (pid, device time ns) belongs to
// can be the same lifetime; false vetoes the pairing whatever the time
// says (nil: no veto). The time windows only remain for a legacy fact (no
// "ev", written by 0.9.x): legacyAnrWindow, legacyCrashWindow; a fact with
// "ev" is bounded by anrEvidenceCap and, for a Java or native crash, by
// the incarnation alone.
func MatchDropboxIn(list []*Crash, e DropboxEntry, boot string, jumps []ClockShift, same func(x *Crash, pid int, ns int64) bool) (c *Crash, isNew bool) {
	kind := ""
	switch {
	case e.Tag == "SYSTEM_TOMBSTONE":
		kind = "native"
	case strings.HasSuffix(e.Tag, "_anr"):
		kind = "anr"
	case strings.HasSuffix(e.Tag, "_native_crash"):
		kind = "native"
	case strings.HasSuffix(e.Tag, "_crash"):
		kind = "java"
	default:
		return nil, false
	}
	proc := headerField(e.Body, "Process")
	pid, _ := strconv.Atoi(headerField(e.Body, "PID"))
	tomb := ""
	if e.Tag == "SYSTEM_TOMBSTONE" {
		// no Process/PID header: take them from the tombstone itself
		if i := strings.Index(e.Body, "*** *** ***"); i >= 0 {
			tomb = e.Body[i:]
		}
		for _, ln := range strings.Split(tomb, "\n") {
			if m := reTombPID.FindStringSubmatch(strings.TrimSpace(ln)); m != nil {
				pid, _ = strconv.Atoi(m[1])
				proc = m[4]
				break
			}
		}
		if tomb == "" {
			return nil, false
		}
		if len(tomb) > MaxTombstone {
			tomb = tomb[:MaxTombstone] + "\n…[truncated by android-log-mcp]"
		}
	}
	var best, far *Crash
	var bestD, farD int64 = 1 << 62, 1 << 62
	var bestShifted, tie, taken bool
	for _, x := range list {
		if x.Kind != kind {
			continue
		}
		if boot != "" && x.Boot != "" && x.Boot != boot {
			continue
		}
		if pid != 0 && x.PID != 0 && x.PID != pid {
			continue
		}
		if proc != "" && x.Process != "" && x.Process != proc && pkgOf(x.Process) != pkgOf(proc) {
			continue
		}
		if same != nil && !same(x, pid, firstNonZero(e.BodyTimeNs, e.TimeNs)) {
			continue // another lifetime of that pid (design 3.1.1)
		}
		fresh := x.HasEvidence() && !x.synthOnly()
		d, shifted := dropboxDelta(x.TimeNs, x.Seq, e.TimeNs, jumps)
		if e.BodyTimeNs != 0 {
			// the body time is the real one, the header time may have been
			// re-dated by the device (issue #152); either may match
			if db, sb := dropboxDelta(x.TimeNs, x.Seq, e.BodyTimeNs, jumps); db < d || db == d && !sb && shifted {
				d, shifted = db, sb
			}
		}
		win := legacyCrashWindow
		if fresh {
			win = anrEvidenceCap // veto cap, not the matcher
		}
		inWin := d < win
		if kind == "anr" {
			if !fresh {
				win = legacyAnrWindow
				inWin = d < win
			}
			if fresh {
				// a fact written by the ordinal pairing (issue #225): the
				// 120 s window gives way to the veto cap, a different
				// reason is a different ANR, and an ANR logged after the
				// entry (or of a later fact of the process) is not its source
				if ok, _ := ReasonCompatible(x.Headline, headerField(e.Body, "Subject")); !ok {
					continue
				}
				if et := firstNonZero(e.BodyTimeNs, e.TimeNs); len(jumps) == 0 && et != 0 {
					if x.TimeNs > et+anrEntrySlack {
						continue
					}
					later := false
					for _, y := range list {
						if y != x && y.Kind == "anr" && y.PID == x.PID && y.Boot == x.Boot && y.HasEvidence() && y.Seq > x.Seq && y.TimeNs <= et+anrEntrySlack {
							later = true
							break
						}
					}
					if later && !x.hasDropboxSource() {
						continue // a later ANR of the process is anchored before the entry: the entry is its (or a later one's)
					}
				}
			}
			// One DropBox entry per ANR (issue #176): an ANR that already
			// has its entry is not offered another one.
			if x.hasDropboxSource() {
				if inWin {
					taken = true
				}
				continue
			}
			// The entry is written after the ANR was declared, so with only
			// its header time to go by, an ANR logged clearly after the
			// entry is the next ANR of that process, not the source (not used when
			// clock jumps are stored: the two readings may sit on either side).
			if e.BodyTimeNs == 0 && len(jumps) == 0 && x.TimeNs > e.TimeNs+anrEntrySlack && inWin {
				d += win // ranks after every ANR logged before the entry
			}
		}
		if inWin && d < bestD {
			best, bestD, bestShifted, tie = x, d, shifted, false
		} else if inWin && d == bestD && best != x && (shifted || bestShifted) {
			tie = true
		} else if d >= win && boot != "" && x.Boot == boot && kind != "anr" && pid != 0 && x.PID == pid &&
			proc != "" && x.Process == proc && (far == nil || d < farD) {
			far, farD = x, d
		}
	}
	if tie {
		best, far = nil, nil
	}
	if best == nil {
		best = far
	}
	if best == nil && taken {
		return nil, false // its ANR already has an entry: not a new crash
	}
	src := "dropbox:" + e.Tag
	if best != nil {
		cp := *best
		cp.addSource(src)
		switch kind {
		case "anr":
			if tr := anrTraces(e.Body); tr != "" && cp.Traces == "" {
				cp.Traces = tr
			}
			if cp.Headline == "" {
				cp.Headline = headerField(e.Body, "Subject")
			}
			if cp.CPU == "" {
				if i := strings.Index(e.Body, "CPU usage"); i >= 0 {
					cp.CPU = firstLines(e.Body[i:], 10)
				}
			}
		default:
			if tomb != "" {
				cp.Tombstone = tomb
				if !strings.Contains(cp.Body, "backtrace:") {
					h := cp.Headline
					cp.Headline = ""
					ParseTombstoneInto(&cp, tomb)
					if cp.Headline == "" {
						cp.Headline = h
					}
					cp.Body = crashingThread(tomb)
				}
			} else if cp.Dropbox == "" && (len(cp.Body) < 200 || strings.Contains(cp.Body, "stack not in the log buffers") || !strings.Contains(cp.Body, "backtrace:") && kind == "native") {
				cp.Dropbox = e.Body
			}
		}
		return &cp, false
	}
	if e.Note != "" && e.Body == "" {
		return nil, false
	}
	c = &Crash{Kind: kind, TimeNs: e.RealTimeNs(), PID: pid, Process: proc, Package: pkgOf(proc), Sources: []string{src}}
	switch kind {
	case "java":
		j := JavaCrashFromMessage(stripHeaders(e.Body))
		c.Headline, c.Body, c.Signature = j.Headline, j.Body, j.Signature
	case "native":
		if tomb != "" {
			ParseTombstoneInto(c, tomb)
			c.Body = crashingThread(tomb)
			c.Tombstone = tomb
			return c, true
		}
		ParseTombstoneInto(c, stripHeaders(e.Body))
	case "anr":
		c.Headline = headerField(e.Body, "Subject")
		c.Body = strings.SplitN(e.Body, "\n----- pid", 2)[0]
		c.Traces = anrTraces(e.Body)
		if i := strings.Index(e.Body, "CPU usage"); i >= 0 {
			c.CPU = firstLines(e.Body[i:], 10)
		}
	}
	c.Dropbox = e.Body
	return c, true
}

func stripHeaders(body string) string {
	parts := strings.SplitN(body, "\n\n", 2)
	if len(parts) == 2 && isDropboxHeader(strings.SplitN(parts[0], "\n", 2)[0]) {
		return parts[1]
	}
	return body
}

// anrTraces returns the thread-dump part of a dropbox ANR entry.
func anrTraces(body string) string {
	if i := strings.Index(body, "\n----- pid "); i >= 0 {
		return body[i+1:]
	}
	if i := strings.Index(body, "\n\"main\""); i >= 0 {
		return body[i+1:]
	}
	return ""
}

// crashingThread cuts a full tombstone down to the header and the crashing
// thread (up to the first "--- --- ---" separator or the stack dump).
func crashingThread(tomb string) string {
	end := len(tomb)
	for _, sep := range []string{"\n--- --- ---", "\nstack:", "\nmemory near ", "\nmemory map"} {
		if i := strings.Index(tomb, sep); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimRight(tomb[:end], "\n")
}

// AdoptDropbox handles the reverse order of MatchDropbox: a crash first known
// only from DropBox (e.g. fetched while the logcat backlog was still being
// replayed) is later seen in logcat. The new logcat crash c (ID == "") takes
// over the DropBox entry's ID and its DropBox-only details. Returns true if
// an entry was adopted. jumps are the stored clock jumps (see
// MatchDropboxBoot); the same boot, kind and pid still have to agree.
func AdoptDropbox(list []*Crash, c *Crash, jumps []ClockShift) bool {
	return AdoptDropboxIn(list, c, jumps, nil)
}

// AdoptDropboxIn is AdoptDropbox with the incarnation veto: same(x, c)
// reports whether the DropBox-only fact x and the logcat crash c can be one
// process lifetime. A twin that carries "ev" is bounded by anrEvidenceCap
// (an ANR also by its reason); a legacy twin (0.9.x) keeps the old window.
func AdoptDropboxIn(list []*Crash, c *Crash, jumps []ClockShift, same func(x, c *Crash) bool) bool {
	if c.ID != "" || c.PID == 0 {
		return false
	}
	var best *Crash
	var bestD int64 = 1 << 62
	var bestShifted, tie bool
	for _, x := range list {
		if x.Kind != c.Kind || x.PID != c.PID || x.Boot != "" && c.Boot != "" && x.Boot != c.Boot || !dropboxOnly(x) {
			continue
		}
		if same != nil && !same(x, c) {
			continue
		}
		win := legacyCrashWindow
		if c.Kind == "anr" {
			win = legacyAnrWindow
		}
		if x.HasEvidence() && !x.synthOnly() {
			win = anrEvidenceCap
			if c.Kind == "anr" {
				if ok, _ := ReasonCompatible(x.Headline, c.Headline); !ok {
					continue
				}
			}
		}
		d, shifted := dropboxDelta(c.TimeNs, c.Seq, x.TimeNs, jumps)
		if d < win && d < bestD {
			best, bestD, bestShifted, tie = x, d, shifted, false
		} else if d < win && d == bestD && best != x && (shifted || bestShifted) {
			tie = true
		}
	}
	if best == nil || tie {
		return false
	}
	c.ID = best.ID
	for _, s := range best.Sources {
		c.addSource(s)
	}
	if c.Traces == "" {
		c.Traces = best.Traces
	}
	if c.CPU == "" {
		c.CPU = best.CPU
	}
	if c.Tombstone == "" {
		c.Tombstone = best.Tombstone
	}
	if c.Dropbox == "" && best.Tombstone == "" && (c.Kind != "native" || !strings.Contains(c.Body, "backtrace:")) {
		c.Dropbox = best.Dropbox
	}
	if c.Location == "" {
		c.Location = best.Location
	}
	return true
}

func dropboxOnly(c *Crash) bool {
	if len(c.Sources) == 0 {
		return false
	}
	for _, s := range c.Sources {
		if !strings.HasPrefix(s, "dropbox:") {
			return false
		}
	}
	return true
}

// KeepDropbox carries DropBox-derived details of the previously stored
// version of a crash over to an update from the logcat detector (whose copy
// never saw the DropBox merge). Without this a later logcat update (e.g. the
// am_crash line arriving after the DropBox fetch) would drop them.
func KeepDropbox(c, old *Crash) {
	if old == nil || old.ID != c.ID {
		return
	}
	for _, s := range old.Sources {
		if strings.HasPrefix(s, "dropbox:") {
			c.addSource(s)
		}
	}
	if c.Traces == "" {
		c.Traces = old.Traces
	}
	if c.CPU == "" {
		c.CPU = old.CPU
	}
	if c.Tombstone == "" {
		c.Tombstone = old.Tombstone
	}
	if c.Dropbox == "" {
		c.Dropbox = old.Dropbox
	}
	if c.Location == "" {
		c.Location = old.Location
	}
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
