// Package logcat parses `adb logcat -b main,system,crash,events -v threadtime
// -v epoch -v uid -v usec -v printable -D` output into model.Records.
package logcat

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Args returns the logcat arguments used for recording. -D (dividers) makes
// logcat print "--------- switch to X" on every buffer change so each line
// can be attributed to its buffer.
func Args(resumeFrom string) []string {
	a := []string{"logcat", "-b", "main,system,crash,events", "-v", "threadtime", "-v", "epoch", "-v", "uid", "-v", "usec", "-v", "printable", "-D"}
	if resumeFrom != "" {
		a = append(a, "-T", resumeFrom)
	}
	return a
}

// FormatResumeTime renders a -T argument ("sec.usec") for a unix-ns time.
func FormatResumeTime(ns int64) string {
	return strconv.FormatInt(ns/1e9, 10) + "." + pad6(ns%1e9/1e3)
}

func pad6(n int64) string {
	s := strconv.FormatInt(n, 10)
	return strings.Repeat("0", 6-len(s)) + s
}

// Known short uid names printed by logprint when getpwuid() name ≤ 5 chars.
var uidNames = map[string]int{"root": 0, "radio": 1001, "input": 1004, "audio": 1005, "media": 1013, "nfc": 1027, "shell": 2000, "cache": 2001, "diag": 2002, "log": 1007, "wifi": 1010, "drm": 1019, "gps": 1021, "vpn": 1016, "keystore": 1017, "dhcp": 1014, "mtp": 1024, "dns": 1051, "lmkd": 1069, "llkd": 1070}

// Line is one parsed physical output line.
type Line struct {
	Divider string // non-empty for "--------- beginning of X"/"switch to X"
	OK      bool   // a log line with a header
	TimeNs  int64
	UID     int
	PID     int
	TID     int
	Level   model.Level
	Tag     string
	Msg     string
}

// ParseLine parses one physical line (without trailing newline). loc and
// year are used only for the non-epoch "MM-DD HH:MM:SS.mmm" fallback.
func ParseLine(s string, loc *time.Location, year int) Line {
	s = strings.TrimRight(s, "\r")
	if strings.HasPrefix(s, "--------- ") {
		rest := s[len("--------- "):]
		for _, p := range []string{"beginning of ", "switch to "} {
			if strings.HasPrefix(rest, p) {
				return Line{Divider: strings.TrimSpace(rest[len(p):])}
			}
		}
	}
	i := skipSpaces(s, 0)
	// timestamp
	var l Line
	j := i
	for j < len(s) && s[j] != ' ' {
		j++
	}
	tsTok := s[i:j]
	if strings.Count(tsTok, "-") == 1 && len(tsTok) == 5 || strings.Count(tsTok, "-") == 2 && len(tsTok) == 10 {
		// "MM-DD HH:MM:SS.mmm" or "YYYY-MM-DD HH:MM:SS.mmm" (threadtime without epoch)
		k := skipSpaces(s, j)
		e := k
		for e < len(s) && s[e] != ' ' {
			e++
		}
		ns, ok := parseDateTime(tsTok, s[k:e], loc, year)
		if !ok {
			return Line{}
		}
		l.TimeNs = ns
		j = e
	} else {
		ns, ok := parseEpoch(tsTok)
		if !ok {
			return Line{}
		}
		l.TimeNs = ns
	}
	// fields until the 1-char level token
	var fields []string
	k := j
	for len(fields) < 4 {
		k = skipSpaces(s, k)
		e := k
		for e < len(s) && s[e] != ' ' {
			e++
		}
		if e == k {
			return Line{}
		}
		tok := s[k:e]
		k = e
		if len(tok) == 1 && isLevel(tok[0]) && len(fields) >= 2 {
			l.Level = model.Level(tok[0])
			if l.Level == 'A' {
				l.Level = 'F'
			}
			break
		}
		fields = append(fields, tok)
	}
	if l.Level == 0 || len(fields) < 2 || len(fields) > 3 {
		return Line{}
	}
	l.UID = -1
	if len(fields) == 3 {
		if n, err := strconv.Atoi(fields[0]); err == nil {
			l.UID = n
		} else if n, ok := uidNames[fields[0]]; ok {
			l.UID = n
		}
		fields = fields[1:]
	}
	var err1, err2 error
	l.PID, err1 = strconv.Atoi(fields[0])
	l.TID, err2 = strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return Line{}
	}
	// " <tag padded to 8>: <msg>"
	if k < len(s) && s[k] == ' ' {
		k++
	}
	rest := s[k:]
	if c := strings.Index(rest, ": "); c >= 0 {
		l.Tag = strings.TrimRight(rest[:c], " ")
		l.Msg = rest[c+2:]
	} else if strings.HasSuffix(rest, ":") {
		l.Tag = strings.TrimRight(rest[:len(rest)-1], " ")
	} else {
		return Line{}
	}
	l.Tag = UnescapePrintable(l.Tag)
	l.Msg = UnescapePrintable(l.Msg)
	l.OK = true
	return l
}

func isLevel(c byte) bool {
	switch c {
	case 'V', 'D', 'I', 'W', 'E', 'F', 'A', 'S':
		return true
	}
	return false
}

func skipSpaces(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

func parseEpoch(t string) (int64, bool) {
	dot := strings.IndexByte(t, '.')
	if dot <= 0 {
		return 0, false
	}
	sec, err := strconv.ParseInt(t[:dot], 10, 64)
	if err != nil {
		return 0, false
	}
	frac := t[dot+1:]
	if len(frac) == 0 || len(frac) > 9 {
		return 0, false
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, false
	}
	for n := len(frac); n < 9; n++ {
		f *= 10
	}
	// seconds*1e9 must fit an int64 (until the year 2262): a later epoch (a
	// device clock set far ahead, or a corrupt line) is not a time, not a
	// wrapped negative one (issue #195)
	const maxSec, maxFrac = math.MaxInt64 / 1_000_000_000, math.MaxInt64 % 1_000_000_000
	if sec > maxSec || sec < -maxSec || (sec == maxSec && f > maxFrac) {
		return 0, false
	}
	return sec*1e9 + f, true
}

func parseDateTime(date, clock string, loc *time.Location, year int) (int64, bool) {
	if loc == nil {
		loc = time.Local
	}
	if len(date) == 5 {
		date = strconv.Itoa(year) + "-" + date
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999", date+" "+clock, loc)
	if err != nil {
		return 0, false
	}
	if y := t.Year(); y < 1678 || y > 2261 { // UnixNano is undefined outside int64 nanoseconds (issue #195)
		return 0, false
	}
	return t.UnixNano(), true
}
