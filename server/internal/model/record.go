// Package model defines the stored log record shared by all packages.
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Level is a logcat priority letter: V D I W E F (A is normalized to F).
type Level byte

const (
	Verbose Level = 'V'
	Debug   Level = 'D'
	Info    Level = 'I'
	Warn    Level = 'W'
	Error   Level = 'E'
	Fatal   Level = 'F'
)

// Rank orders levels: V=1 … F/A=6; unknown = 0.
func (l Level) Rank() int {
	switch l {
	case 'V':
		return 1
	case 'D':
		return 2
	case 'I':
		return 3
	case 'W':
		return 4
	case 'E':
		return 5
	case 'F', 'A':
		return 6
	}
	return 0
}

// Name returns the Android Studio level name.
func (l Level) Name() string {
	switch l {
	case 'V':
		return "VERBOSE"
	case 'D':
		return "DEBUG"
	case 'I':
		return "INFO"
	case 'W':
		return "WARN"
	case 'E':
		return "ERROR"
	case 'F', 'A':
		return "ASSERT"
	}
	return "?"
}

// LevelFromRank is the inverse of Rank.
func LevelFromRank(r int) Level {
	return Level("?VDIWEF"[max(0, min(r, 6))])
}

// Record is one (possibly multi-line) logcat entry.
type Record struct {
	Seq     uint64 `json:"s"`
	Boot    string `json:"b"` // short boot id (first 8 hex chars)
	TimeNs  int64  `json:"t"` // device wall clock, unix ns (µs precision)
	UID     int    `json:"u"` // -1 when unknown
	PID     int    `json:"p"`
	TID     int    `json:"i"`
	Level   Level  `json:"l"`
	Tag     string `json:"g"`
	Msg     string `json:"m"`
	Buffer  string `json:"f,omitempty"` // main system crash events …
	Process string `json:"n,omitempty"` // process name resolved at ingest
	Package string `json:"k,omitempty"` // application id resolved at ingest

	line string // cached AS canonical line (not persisted)
}

func (r *Record) Time() time.Time { return time.Unix(0, r.TimeNs) }

// AppID mirrors Android Studio's header.applicationId fallback chain.
func (r *Record) AppID() string {
	if r.Package != "" {
		return r.Package
	}
	if r.Process != "" {
		return r.Process
	}
	return ""
}

// Line is Android Studio's canonical line used by `line:` and bare words:
// "yyyy-MM-dd HH:mm:ss.SSS pid-tid tag appId L: message" in the device zone.
func (r *Record) Line(loc *time.Location) string {
	if r.line != "" {
		return r.line
	}
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder
	b.Grow(len(r.Msg) + len(r.Tag) + 64)
	b.WriteString(r.Time().In(loc).Format("2006-01-02 15:04:05.000 "))
	b.WriteString(itoa(r.PID))
	b.WriteByte('-')
	b.WriteString(itoa(r.TID))
	b.WriteByte(' ')
	b.WriteString(r.Tag)
	b.WriteByte(' ')
	b.WriteString(r.AppID())
	b.WriteByte(' ')
	lv := r.Level
	if lv == 'F' {
		lv = 'A' // AS prints ASSERT's priority letter
	}
	b.WriteByte(byte(lv))
	b.WriteString(": ")
	b.WriteString(r.Msg)
	r.line = b.String()
	return r.line
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var uidNames = map[int]string{0: "root", 1000: "system", 1001: "radio", 1002: "bluetooth", 1007: "log", 1010: "wifi", 1013: "media", 1041: "audioserver", 1047: "cameraserver", 1073: "network_stack", 2000: "shell", 9999: "nobody"}

// UIDName renders a uid for humans ("shell", "system", "u0_a146", "1234").
func UIDName(uid int) string {
	if n, ok := uidNames[uid]; ok {
		return n
	}
	if a := uid % 100000; a >= 10000 && a < 20000 {
		return fmt.Sprintf("u%d_a%d", uid/100000, a-10000)
	}
	return strconv.Itoa(uid)
}
