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

// CompactAux trims the append-only auxiliary files of a device directory
// (writer only) and returns the bytes saved:
//   - procs.jsonl and crashes.jsonl keep only the newest version of each
//     entry (the readers already use "last version wins"), in the order the
//     entries first appeared, so LoadProcs / LoadCrashes return the same;
//   - ended processes, crashes and dropbox_seen keys older than the
//     retention window (Config.MaxAge, ANDROID_LOG_MCP_MAX_AGE) are dropped.
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
			var p procs.Proc
			if json.Unmarshal(b, &p) != nil {
				return "", false
			}
			k := p.ID
			if k == "" {
				k = fmt.Sprintf("%s|%d|%d|%s", p.Boot, p.PID, p.StartNs, p.Name)
			}
			return k, !(p.EndNs > 0 && p.EndNs < ageCut)
		})
	}))
	add(st.CompactAux("crashes", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			var c analysis.Crash
			if json.Unmarshal(b, &c) != nil || c.ID == "" {
				return "", false
			}
			return c.ID, c.TimeNs == 0 || c.TimeNs >= ageCut
		})
	}))
	add(st.CompactAux("dropbox_seen", func(lines [][]byte) [][]byte {
		return lastVersions(lines, func(b []byte) (string, bool) {
			var key string // "tag|timeNs"
			if json.Unmarshal(b, &key) != nil {
				return "", false
			}
			if i := strings.LastIndexByte(key, '|'); i >= 0 {
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
