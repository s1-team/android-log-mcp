package analysis

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DropboxEntry is one entry of `dumpsys dropbox --print <tag>`.
type DropboxEntry struct {
	Tag    string `json:"tag"`
	TimeNs int64  `json:"t"` // device-local wall clock, second precision
	Body   string `json:"body"`
	Note   string `json:"note,omitempty"` // e.g. "contents lost"
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
	var best *Crash
	var bestD int64 = 1 << 62
	for _, x := range list {
		if x.Kind != kind {
			continue
		}
		if pid != 0 && x.PID != 0 && x.PID != pid {
			continue
		}
		if proc != "" && x.Process != "" && x.Process != proc && pkgOf(x.Process) != pkgOf(proc) {
			continue
		}
		d := abs(x.TimeNs - e.TimeNs)
		if d < 60e9 && d < bestD {
			best, bestD = x, d
		}
	}
	src := "dropbox:" + e.Tag
	if best != nil {
		cp := *best
		cp.addSource(src)
		switch kind {
		case "anr":
			if tr := anrTraces(e.Body); tr != "" {
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
	c = &Crash{Kind: kind, TimeNs: e.TimeNs, PID: pid, Process: proc, Package: pkgOf(proc), Sources: []string{src}}
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
// an entry was adopted.
func AdoptDropbox(list []*Crash, c *Crash) bool {
	if c.ID != "" || c.PID == 0 {
		return false
	}
	var best *Crash
	var bestD int64 = 1 << 62
	for _, x := range list {
		if x.Kind != c.Kind || x.PID != c.PID || x.Boot != "" && c.Boot != "" && x.Boot != c.Boot || !dropboxOnly(x) {
			continue
		}
		if d := abs(x.TimeNs - c.TimeNs); d < 60e9 && d < bestD {
			best, bestD = x, d
		}
	}
	if best == nil {
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
