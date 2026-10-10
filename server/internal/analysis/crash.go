// Package analysis turns raw records into crashes/ANRs and app launches.
package analysis

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// Crash is a Java crash, native crash or ANR. Persisted in crashes.jsonl;
// later lines with the same ID supersede earlier ones.
type Crash struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // java | native | anr
	Boot      string   `json:"boot"`
	TimeNs    int64    `json:"t"`
	Seq       uint64   `json:"seq,omitempty"` // first evidence record (0 = dropbox only)
	PID       int      `json:"pid"`
	Process   string   `json:"process,omitempty"`
	Package   string   `json:"package,omitempty"`
	Thread    string   `json:"thread,omitempty"`
	Headline  string   `json:"headline"`
	Body      string   `json:"body,omitempty"` // stack / tombstone / ANR block
	Signature string   `json:"sig,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Traces    string   `json:"traces,omitempty"` // ANR traces (dropbox)
	CPU       string   `json:"cpu,omitempty"`
	Dropbox   string   `json:"dropbox,omitempty"`   // raw dropbox entry (crash/native when no logcat evidence)
	Tombstone string   `json:"tombstone,omitempty"` // full tombstone (all threads) from DropBox SYSTEM_TOMBSTONE
	Location  string   `json:"location,omitempty"`  // native: first app frame (fallback: #00)
}

// MaxTombstone caps the stored SYSTEM_TOMBSTONE text per crash.
const MaxTombstone = 96 << 10

func (c *Crash) addSource(s string) {
	for _, x := range c.Sources {
		if x == s {
			return
		}
	}
	c.Sources = append(c.Sources, s)
}

// Detector recognizes crashes in the ingest stream (writer process only).
type Detector struct {
	mu sync.Mutex
	// Emit is called for new or updated crashes; the callback assigns IDs
	// (when ID=="") and persists them.
	Emit func(c *Crash)
	// NeedDropbox is called when dropbox should be polled for a tag.
	NeedDropbox func(tag string)

	recent   []*Crash           // last crashes for dedup/merge (bounded)
	amNative []amNative         // am_crash "Native crash" events waiting for the tombstone
	natives  map[int]*Crash     // pending native crash by crashed pid
	tombs    map[int]*tombstone // DEBUG lines by crash_dump pid
	lastTs   int64
}

type amNative struct {
	proc, boot, sig string
	ns              int64
	seq             uint64
}

func (d *Detector) pendingNative(proc string) *Crash {
	for _, c := range d.natives {
		if sameProc(c.Process, proc) {
			return c
		}
	}
	return nil
}

// mergeAmNative attaches a waiting am_crash(Native crash) to c.
func (d *Detector) mergeAmNative(c *Crash) {
	kept := d.amNative[:0]
	for _, a := range d.amNative {
		if sameProc(c.Process, a.proc) && abs(a.ns-c.TimeNs) < 60e9 {
			c.addSource("events:am_crash")
			c.Process, c.Package = a.proc, pkgOf(a.proc)
			continue
		}
		kept = append(kept, a)
	}
	d.amNative = kept
}

type tombstone struct {
	lines   []string
	pid     int // crashed pid from "pid: N, tid: ..." line
	lastTs  int64
	firstTs int64
	seq     uint64
	boot    string
}

func NewDetector() *Detector {
	return &Detector{natives: map[int]*Crash{}, tombs: map[int]*tombstone{}}
}

var (
	reProcessPID = regexp.MustCompile(`(?m)^Process: (\S+), PID: (\d+)`)
	reFatalSig   = regexp.MustCompile(`^Fatal signal (\d+) \((\w+)\)(?:, code (-?\d+) \((\w+)\))?(?:, fault addr (0x[0-9a-fA-F]+))?(?: in tid (\d+) \(([^)]*)\))?(?:, pid (\d+) \(([^)]*)\))?`)
	reTombPID    = regexp.MustCompile(`^pid: (\d+), tid: (\d+), name: (.*?)\s+>>> (.*?) <<<`)
	reANRIn      = regexp.MustCompile(`^ANR in (\S+)`)
	reANRPid     = regexp.MustCompile(`(?m)^PID: (\d+)`)
	reANRReason  = regexp.MustCompile(`(?m)^(?:Reason|Subject): (.*)$`)
	reTombSig    = regexp.MustCompile(`^signal \d+ \((\w+)\)(?:, code -?\d+ \((\w+)\))?(?:, fault addr (\S+))?`)
	reAppFrame   = regexp.MustCompile(`^\s*at ([\w$.<>]+)\(`)
)

func (d *Detector) remember(c *Crash) {
	d.recent = append(d.recent, c)
	if len(d.recent) > 64 {
		d.recent = d.recent[len(d.recent)-64:]
	}
}

func (d *Detector) findRecent(kind string, pid int, ns int64, window int64) *Crash {
	for i := len(d.recent) - 1; i >= 0; i-- {
		c := d.recent[i]
		if c.Kind == kind && c.PID == pid && abs(c.TimeNs-ns) <= window {
			return c
		}
	}
	return nil
}

// findRecentProc finds a crash of kind for the same app process near ns.
func (d *Detector) findRecentProc(kind, proc string, ns int64, window int64) *Crash {
	for i := len(d.recent) - 1; i >= 0; i-- {
		c := d.recent[i]
		if c.Kind == kind && sameProc(c.Process, proc) && abs(c.TimeNs-ns) <= window {
			return c
		}
	}
	return nil
}

func sameProc(a, b string) bool {
	return a != "" && b != "" && (a == b || shortProc(a) == shortProc(b))
}

// shortProc: tombstones and libc print the 15-char thread name
// ("example.logtest"), AMS the full process name.
func shortProc(p string) string {
	if len(p) > 15 {
		return p[len(p)-15:]
	}
	return p
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Feed processes one annotated, sequenced record.
func (d *Detector) Feed(r *model.Record) {
	d.mu.Lock()
	var emit []*Crash
	var dropbox []string
	defer func() {
		d.mu.Unlock()
		for _, c := range emit {
			if d.Emit != nil {
				d.Emit(c)
			}
		}
		for _, t := range dropbox {
			if d.NeedDropbox != nil {
				d.NeedDropbox(t)
			}
		}
	}()
	d.lastTs = max(d.lastTs, r.TimeNs)
	emit = append(emit, d.expireLocked(r.TimeNs)...)

	switch {
	case r.Tag == "AndroidRuntime" && r.Level == model.Error && strings.HasPrefix(r.Msg, "FATAL EXCEPTION"):
		j := JavaCrashFromMessage(r.Msg)
		pid := j.PID
		if pid == 0 {
			pid = r.PID
		}
		c := d.findRecent("java", pid, r.TimeNs, 15e9)
		if c == nil {
			c = &Crash{Kind: "java", Boot: r.Boot, TimeNs: r.TimeNs, Seq: r.Seq, PID: pid}
			d.remember(c)
		} else {
			// am_crash came first: upgrade it with the real stack, keep its ID
			c.TimeNs, c.Seq = r.TimeNs, r.Seq
		}
		c.Thread, c.Headline, c.Body, c.Signature = j.Thread, j.Headline, j.Body, j.Signature
		c.Process = j.Process
		if c.Process == "" {
			c.Process = r.Process
		}
		c.Package = r.Package
		if c.Package == "" {
			c.Package = pkgOf(c.Process)
		}
		c.addSource("logcat:" + r.Buffer)
		emit = append(emit, c)
		dropbox = append(dropbox, "data_app_crash")

	case r.Tag == "am_crash":
		// API 29: [PID,User,Process,Flags,Exception,Message,File,Line];
		// AOSP tags file: [User,PID,…] — AmCrashPID decides by value.
		f := logcat.EventFields(r.Msg, 9)
		pid, _, proc, ok := procs.AmCrashPID(r.Msg)
		if !ok || len(f) < 6 {
			break
		}
		exc, msg := f[4], f[5]
		if exc == "Native crash" {
			// pid is system_server's here; the tombstone/libc evidence is
			// keyed by the app. Merge into that native crash.
			if c := d.findRecentProc("native", proc, r.TimeNs, 60e9); c != nil {
				c.addSource("events:am_crash")
				if c.Process == "" || len(c.Process) < len(proc) {
					c.Process, c.Package = proc, pkgOf(proc)
				}
				emit = append(emit, c)
			} else if c := d.pendingNative(proc); c != nil {
				c.addSource("events:am_crash")
				c.Process, c.Package = proc, pkgOf(proc)
			} else {
				d.amNative = append(d.amNative, amNative{proc: proc, ns: r.TimeNs, seq: r.Seq, boot: r.Boot, sig: msg})
			}
			dropbox = append(dropbox, "data_app_native_crash")
			break
		}
		if c := d.findRecent("java", pid, r.TimeNs, 15e9); c != nil {
			c.addSource("events:am_crash")
			emit = append(emit, c)
			break
		}
		c := &Crash{Kind: "java", Boot: r.Boot, TimeNs: r.TimeNs, Seq: r.Seq, PID: pid, Process: proc, Package: pkgOf(proc),
			Headline: strings.TrimSpace(exc + ": " + msg)}
		c.Signature = exc + "@" + msg
		if len(f) >= 8 {
			c.Body = fmt.Sprintf("%s: %s\n\tat %s:%s (only the am_crash event was recorded; stack not in the log buffers)", exc, msg, f[6], f[7])
			c.Signature = exc + "@" + f[6] + ":" + f[7]
		}
		c.addSource("events:am_crash")
		d.remember(c)
		emit = append(emit, c)
		dropbox = append(dropbox, "data_app_crash")

	case r.Tag == "libc" && r.Level.Rank() == 6 && strings.HasPrefix(r.Msg, "Fatal signal"):
		c := &Crash{Kind: "native", Boot: r.Boot, TimeNs: r.TimeNs, Seq: r.Seq, PID: r.PID, Process: r.Process, Package: r.Package}
		if m := reFatalSig.FindStringSubmatch(r.Msg); m != nil {
			c.Headline = m[2]
			if m[4] != "" {
				c.Headline += " (" + m[4] + ")"
			}
			if m[5] != "" && m[1] != "6" {
				c.Headline += " fault addr " + m[5]
			}
			if m[7] != "" {
				c.Thread = m[7]
			}
			if m[8] != "" {
				c.PID, _ = strconv.Atoi(m[8])
				if c.Process == "" {
					c.Process = m[9]
				}
			}
		} else {
			c.Headline = r.Msg
		}
		c.Body = r.Msg
		if c.Package == "" {
			c.Package = pkgOf(c.Process)
		}
		c.addSource("logcat:" + r.Buffer)
		d.natives[c.PID] = c
		d.mergeAmNative(c)

	case r.Tag == "DEBUG" && r.Level.Rank() == 6:
		t := d.tombs[r.PID]
		if strings.HasPrefix(strings.TrimSpace(r.Msg), "*** *** ***") {
			if t != nil {
				emit = append(emit, d.finishTombLocked(r.PID)...)
			}
			t = &tombstone{firstTs: r.TimeNs, seq: r.Seq, boot: r.Boot}
			d.tombs[r.PID] = t
		}
		if t == nil {
			t = &tombstone{firstTs: r.TimeNs, seq: r.Seq, boot: r.Boot}
			d.tombs[r.PID] = t
		}
		t.lines = append(t.lines, r.Msg)
		t.lastTs = r.TimeNs
		if m := reTombPID.FindStringSubmatch(r.Msg); m != nil {
			t.pid, _ = strconv.Atoi(m[1])
		}

	case r.Tag == "am_anr":
		// [User,pid,Package Name,Flags,reason]
		f := logcat.EventFields(r.Msg, 5)
		if len(f) >= 5 {
			pid, _ := strconv.Atoi(f[1])
			c := d.findRecent("anr", pid, r.TimeNs, 30e9)
			if c == nil {
				c = &Crash{Kind: "anr", Boot: r.Boot, TimeNs: r.TimeNs, Seq: r.Seq, PID: pid, Process: f[2], Package: pkgOf(f[2]), Headline: f[4]}
				d.remember(c)
			} else if c.Headline == "" {
				c.Headline = f[4]
			}
			c.addSource("events:am_anr")
			emit = append(emit, c)
			dropbox = append(dropbox, "data_app_anr")
		}

	case (r.Tag == "ActivityManager" || r.Tag == "ActivityManagerService" || r.Tag == "AnrHelper") && strings.HasPrefix(r.Msg, "ANR in "):
		m := reANRIn.FindStringSubmatch(r.Msg)
		proc := m[1]
		pid := 0
		if pm := reANRPid.FindStringSubmatch(r.Msg); pm != nil {
			pid, _ = strconv.Atoi(pm[1])
		}
		c := d.findRecent("anr", pid, r.TimeNs, 30e9)
		if c == nil {
			c = &Crash{Kind: "anr", Boot: r.Boot, TimeNs: r.TimeNs, Seq: r.Seq, PID: pid, Process: proc, Package: pkgOf(proc)}
			d.remember(c)
		}
		if rm := reANRReason.FindStringSubmatch(r.Msg); rm != nil && c.Headline == "" {
			c.Headline = rm[1]
		}
		c.Body = r.Msg
		if i := strings.Index(r.Msg, "CPU usage"); i >= 0 {
			c.CPU = firstLines(r.Msg[i:], 10)
		}
		c.addSource("logcat:" + r.Buffer)
		emit = append(emit, c)
		dropbox = append(dropbox, "data_app_anr")
	}
}

// Tick finalizes pending native crashes after quiet periods (call ~1/s with
// the newest device time seen, or 0 to use the last record time).
func (d *Detector) Tick(nowNs int64) {
	d.mu.Lock()
	if nowNs == 0 {
		nowNs = d.lastTs
	}
	out := d.expireLocked(nowNs)
	d.mu.Unlock()
	for _, c := range out {
		if d.Emit != nil {
			d.Emit(c)
		}
	}
	if len(out) > 0 && d.NeedDropbox != nil {
		d.NeedDropbox("data_app_native_crash")
	}
}

const quietNs = 3e9

func (d *Detector) expireLocked(now int64) []*Crash {
	var out []*Crash
	for dpid, t := range d.tombs {
		if now-t.lastTs > quietNs {
			out = append(out, d.finishTombLocked(dpid)...)
		}
	}
	for pid, c := range d.natives {
		if now-c.TimeNs > quietNs+2e9 {
			delete(d.natives, pid)
			if c.Signature == "" {
				c.Signature = c.Headline
			}
			d.mergeAmNative(c)
			d.remember(c)
			out = append(out, c)
		}
	}
	kept := d.amNative[:0]
	for _, a := range d.amNative {
		if now-a.ns < 30e9 {
			kept = append(kept, a)
			continue
		}
		if d.findRecentProc("native", a.proc, a.ns, 60e9) != nil {
			continue
		}
		c := &Crash{Kind: "native", Boot: a.boot, TimeNs: a.ns, Seq: a.seq, Process: a.proc, Package: pkgOf(a.proc),
			Headline: "Native crash: " + a.sig, Signature: "native@" + a.proc + ":" + a.sig,
			Body: "only ActivityManager's am_crash event was recorded (no libc/DEBUG tombstone lines in the log buffers); see DropBox data_app_native_crash / SYSTEM_TOMBSTONE"}
		c.addSource("events:am_crash")
		d.remember(c)
		out = append(out, c)
	}
	d.amNative = kept
	return out
}

func (d *Detector) finishTombLocked(dpid int) []*Crash {
	t := d.tombs[dpid]
	delete(d.tombs, dpid)
	if t == nil || len(t.lines) == 0 {
		return nil
	}
	body := strings.Join(t.lines, "\n")
	c := d.natives[t.pid]
	if c != nil {
		delete(d.natives, t.pid)
	} else {
		c = &Crash{Kind: "native", Boot: t.boot, TimeNs: t.firstTs, Seq: t.seq, PID: t.pid}
	}
	c.addSource("logcat:tombstone")
	ParseTombstoneInto(c, body)
	d.mergeAmNative(c)
	d.remember(c)
	return []*Crash{c}
}

// ParseTombstoneInto fills a native crash from tombstone text.
func ParseTombstoneInto(c *Crash, body string) {
	c.Body = body
	var sig, abort string
	var frames []string
	inBT := false
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		switch {
		case reTombPID.MatchString(s):
			m := reTombPID.FindStringSubmatch(s)
			if c.PID == 0 {
				c.PID, _ = strconv.Atoi(m[1])
			}
			if c.Process == "" || len(c.Process) < len(m[4]) && sameProc(c.Process, m[4]) {
				c.Process = m[4]
			}
			if c.Thread == "" {
				c.Thread = m[3]
			}
		case strings.HasPrefix(s, "signal ") && sig == "":
			sig = s
		case strings.HasPrefix(s, "Abort message: ") && abort == "":
			abort = strings.TrimPrefix(s, "Abort message: ")
		case s == "backtrace:":
			inBT = len(frames) == 0
		case inBT && strings.HasPrefix(s, "#"):
			frames = append(frames, s)
		case inBT && s == "":
			if len(frames) > 0 {
				inBT = false
			}
		}
	}
	if c.Package == "" {
		c.Package = pkgOf(c.Process)
	}
	h := c.Headline
	if sig != "" {
		// "signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0"
		// "signal 6 (SIGABRT), code -1 (SI_QUEUE), fault addr --------"
		if m := reTombSig.FindStringSubmatch(sig); m != nil {
			h = m[1]
			if m[2] != "" {
				h += " (" + m[2] + ")"
			}
			if strings.HasPrefix(m[3], "0x") {
				h += " fault addr " + m[3]
			}
		} else {
			h = sig
		}
	}
	if abort != "" {
		h += " abort: " + abort
	}
	if loc := CrashLocation(frames); loc != "" {
		c.Location = loc
		h += " in " + loc
	}
	c.Headline = strings.TrimSpace(h)
	c.Signature = c.Headline
}

// CrashLocation picks the first app frame (code under /data/) of a native
// backtrace, falling back to frame #00 (often libc's abort/syscall).
func CrashLocation(frames []string) string {
	pick := ""
	for _, f := range frames {
		fs := strings.Fields(f)
		if len(fs) >= 4 && strings.HasPrefix(fs[3], "/data/") {
			pick = f
			break
		}
	}
	if pick == "" && len(frames) > 0 {
		pick = frames[0]
	}
	if pick == "" {
		return ""
	}
	// "#02 pc 000000000000086f  /data/app/…/liblogtest.so (Java_…+44) (BuildId: …)"
	fs := strings.Fields(pick)
	if len(fs) < 4 {
		return pick
	}
	lib := fs[3]
	if i := strings.LastIndexByte(lib, '/'); i >= 0 {
		lib = lib[i+1:]
	}
	out := lib
	if j := strings.Index(pick, " ("); j >= 0 {
		fn := pick[j+2:]
		if k := strings.IndexByte(fn, ')'); k >= 0 {
			fn = fn[:k]
		}
		if !strings.HasPrefix(fn, "BuildId") {
			out += " (" + fn + ")"
		}
	}
	return out + " [" + fs[0] + " pc " + strings.TrimLeft(fs[2], "0") + "]"
}

// JavaCrashFromMessage parses an AndroidRuntime FATAL EXCEPTION message (or
// a dropbox data_app_crash body).
func JavaCrashFromMessage(msg string) *Crash {
	c := &Crash{}
	lines := strings.Split(msg, "\n")
	if strings.HasPrefix(lines[0], "FATAL EXCEPTION: ") {
		c.Thread = strings.TrimSpace(strings.TrimPrefix(lines[0], "FATAL EXCEPTION: "))
	}
	if m := reProcessPID.FindStringSubmatch(msg); m != nil {
		c.Process = m[1]
		c.PID, _ = strconv.Atoi(m[2])
	}
	// The exception line: first non-indented line after the header lines.
	start := -1
	for i, ln := range lines {
		if i == 0 && strings.HasPrefix(ln, "FATAL EXCEPTION") {
			continue
		}
		if strings.HasPrefix(ln, "Process: ") || ln == "" || strings.HasPrefix(ln, "\t") || strings.HasPrefix(ln, " ") || isDropboxHeader(ln) {
			continue
		}
		start = i
		break
	}
	if start >= 0 {
		c.Headline = lines[start]
		c.Body = strings.Join(lines[start:], "\n")
	} else {
		c.Body = msg
	}
	c.Signature = JavaSignature(c.Body)
	return c
}

func isDropboxHeader(ln string) bool {
	for _, p := range []string{"PID: ", "UID: ", "Flags: ", "Package: ", "Foreground: ", "Process-Runtime: ", "Build: ", "Loading-Progress: ", "Dropped-Count: ", "Crash-Tag: ", "Subject: ", "Activity: ", "Parent: ", "Frozen: ", "Timestamp: ", "Cmd line: ", "Process: ", "Loading-", "Incremental: ", "isPreviousCrashedInLastHour"} {
		if strings.HasPrefix(ln, p) {
			return true
		}
	}
	return false
}

// JavaSignature = exception class of the root (last Caused by) + first frame.
func JavaSignature(stack string) string {
	lines := strings.Split(stack, "\n")
	exc := ""
	if len(lines) > 0 {
		exc = lines[0]
	}
	frame := ""
	for i, ln := range lines {
		if strings.HasPrefix(ln, "Caused by: ") {
			exc = strings.TrimPrefix(ln, "Caused by: ")
			frame = ""
			for _, l2 := range lines[i+1:] {
				if m := reAppFrame.FindStringSubmatch(l2); m != nil {
					frame = m[1]
					break
				}
			}
		} else if frame == "" {
			if m := reAppFrame.FindStringSubmatch(ln); m != nil {
				frame = m[1]
			}
		}
	}
	if i := strings.IndexByte(exc, ':'); i >= 0 {
		exc = exc[:i]
	}
	return strings.TrimSpace(exc) + "@" + frame
}

func pkgOf(proc string) string {
	if i := strings.IndexByte(proc, ':'); i > 0 {
		return proc[:i]
	}
	return proc
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// MainThreadTrace extracts the "main" thread block from ANR traces, plus the
// block of the thread holding the lock main waits on (if any).
func MainThreadTrace(traces string) (main, holder string) {
	blocks := splitThreads(traces)
	for _, b := range blocks {
		if strings.HasPrefix(b, "\"main\"") {
			main = b
			break
		}
	}
	if main == "" {
		return "", ""
	}
	m := regexp.MustCompile(`held by (?:thread |tid=)(\d+)`).FindStringSubmatch(main)
	if m == nil {
		return main, ""
	}
	want := "tid=" + m[1] + " "
	for _, b := range blocks {
		first := strings.SplitN(b, "\n", 2)[0]
		if strings.Contains(first+" ", want) {
			return main, b
		}
	}
	return main, ""
}

func splitThreads(t string) []string {
	var out []string
	var cur []string
	for _, ln := range strings.Split(t, "\n") {
		if strings.HasPrefix(ln, "\"") {
			if len(cur) > 0 {
				out = append(out, strings.TrimRight(strings.Join(cur, "\n"), "\n"))
			}
			cur = []string{ln}
			continue
		}
		if len(cur) > 0 {
			if strings.TrimSpace(ln) == "" {
				out = append(out, strings.TrimRight(strings.Join(cur, "\n"), "\n"))
				cur = nil
				continue
			}
			cur = append(cur, ln)
		}
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}
