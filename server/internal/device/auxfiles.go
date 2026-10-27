package device

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Aux lines carry the host time they were written ("ht"), so retention of
// the aux files runs on host time like segment retention (#48, #50): a
// device clock set to 1970 or a year ahead must neither drop fresh entries
// nor keep old ones. Readers decode into the plain types and ignore "ht".
type crashAuxLine struct {
	*analysis.Crash
	HostNs int64 `json:"ht,omitempty"`
}

type procLine struct {
	procs.Proc
	HostNs int64 `json:"ht,omitempty"`
}

// seenLine is a dropbox_seen entry: the DropBox entry key "tag|timeNs"
// (device time, as the device lists it) and the host time it was seen.
// Older files hold bare JSON strings.
type seenLine struct {
	Key    string `json:"k"`
	HostNs int64  `json:"ht,omitempty"`
}

func appendProc(st *store.Store, p procs.Proc) {
	st.AppendAux("procs", procLine{Proc: p, HostNs: time.Now().UnixNano()})
}

// parseSeen reads a dropbox_seen line in either format.
func parseSeen(b []byte) (key string, hostNs int64, ok bool) {
	if len(b) > 0 && b[0] == '{' {
		var l seenLine
		if json.Unmarshal(b, &l) != nil || l.Key == "" {
			return "", 0, false
		}
		return l.Key, l.HostNs, true
	}
	if json.Unmarshal(b, &key) != nil || key == "" {
		return "", 0, false
	}
	return key, 0, true
}

// CompactAux trims the append-only auxiliary files of a device directory
// (writer only) and returns the bytes saved:
//   - procs.jsonl, crashes.jsonl and launches.jsonl keep only the newest
//     version of each entry (the readers already use "last version wins"),
//     in the order the entries first appeared;
//   - ended processes, crashes, launches and dropbox_seen keys whose newest version
//     was written longer ago than the retention window (Config.MaxAge,
//     ANDROID_LOG_MCP_MAX_AGE) are dropped. "Ago" is host time (now is the
//     host clock); entries written before host times were stored fall back
//     to their device time.
//
// Leftover *.tmp files from interrupted rewrites are removed too.
func CompactAux(st *store.Store, now time.Time) int64 {
	if !st.IsWriter() {
		return 0
	}
	maxAge := st.Config().MaxAge
	ageCut := now.Add(-maxAge).UnixNano()
	var saved int64
	add := func(before, after int64, _ error) { saved += before - after }

	add(st.CompactAux("procs", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			var l procLine
			if json.Unmarshal(b, &l) != nil {
				return "", false
			}
			p := l.Proc
			k := p.ID
			if k == "" {
				k = fmt.Sprintf("%s|%d|%d|%s", p.Boot, p.PID, p.StartNs, p.Name)
			}
			if p.EndNs <= 0 {
				return k, true // alive
			}
			if l.HostNs > 0 {
				return k, l.HostNs >= ageCut
			}
			return k, p.EndNs >= ageCut // legacy line
		})
	}))
	add(st.CompactAux("crashes", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			l := crashAuxLine{Crash: new(analysis.Crash)}
			if json.Unmarshal(b, &l) != nil || l.ID == "" {
				return "", false
			}
			if l.HostNs > 0 {
				return l.ID, l.HostNs >= ageCut
			}
			return l.ID, l.TimeNs == 0 || l.TimeNs >= ageCut // legacy line
		})
	}))
	add(st.CompactAux("launches", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			l := launchAuxLine{Launch: new(analysis.Launch)}
			if json.Unmarshal(b, &l) != nil || l.ID == "" {
				return "", false
			}
			if l.Ver != 0 && l.Ver != analysis.LaunchFormatVer {
				return "", false // drop stale format; EnsureLaunches rebuilds
			}
			if l.HostNs > 0 {
				return l.ID, l.HostNs >= ageCut
			}
			req := int64(0)
			if l.Launch != nil {
				req = l.RequestNs
			}
			return l.ID, req == 0 || req >= ageCut
		})
	}))
	add(st.CompactAux("dropbox_seen", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			key, host, ok := parseSeen(b)
			if !ok {
				return "", false
			}
			if host > 0 {
				return key, host >= ageCut
			}
			if i := strings.LastIndexByte(key, '|'); i >= 0 { // legacy line
				if ns, err := strconv.ParseInt(key[i+1:], 10, 64); err == nil && ns < ageCut {
					return key, false
				}
			}
			return key, true
		})
	}))
	st.CleanTmp(10 * time.Minute)
	return saved
}

// lastVersions keeps, for each key, the last line with that key, at the
// position where the key first appeared; keep is decided on that last
// version. Lines that do not parse (key "") are dropped.
func lastVersions(lines [][]byte, parse func([]byte) (key string, keep bool)) [][]byte {
	type ent struct {
		line []byte
		keep bool
	}
	byKey := map[string]*ent{}
	var order []string
	for _, l := range lines {
		k, keep := parse(l)
		if k == "" {
			continue
		}
		e := byKey[k]
		if e == nil {
			e = &ent{}
			byKey[k] = e
			order = append(order, k)
		}
		e.line, e.keep = l, keep
	}
	out := make([][]byte, 0, len(order))
	for _, k := range order {
		if e := byKey[k]; e.keep {
			out = append(out, e.line)
		}
	}
	return out
}
