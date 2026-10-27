package mcpserver

import (
	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// packageCrashBounds is the effective time/cursor window for #269 counts
// (intersection of from/to/age and after/before). Zero fields mean open.
// limit / matched-line range / wait do not belong here.
type packageCrashBounds struct {
	fromNs, toNs       int64
	afterSeq, beforeSeq uint64
}

func (b packageCrashBounds) empty() bool {
	return b.fromNs == 0 && b.toNs == 0 && b.afterSeq == 0 && b.beforeSeq == 0
}

// packageCrashCounts returns crash and ANR counts for pkg using crashPkgMatch
// (same as logcat_crashes, issue #269). Both kinds use the same bounds.
// When b is empty, counts are store-known (all retained). Otherwise a crash
// must fall in [fromNs, toNs) by device TimeNs (open ends when 0) and, when
// after/before are set, after the after-cursor and before the before-cursor
// by Seq (same arrival order as search paging; dropbox-only Seq=0 falls back
// to device time vs the cursor line). ok is false when counts cannot be
// computed (omit keys; never fail the primary ok/empty).
func packageCrashCounts(d *device.Device, pkg string, b packageCrashBounds) (crashes, anrs int, ok bool) {
	defer func() {
		if recover() != nil {
			crashes, anrs, ok = 0, 0, false
		}
	}()
	if d == nil || pkg == "" {
		return 0, 0, false
	}
	pk := d.Packages()
	own := procs.NameOwner(pk)
	su := newSharedUID(pkg, pk, d.Procs)
	var afterTime, beforeTime int64
	if b.afterSeq != 0 {
		afterTime = seqTimeNs(d, b.afterSeq)
	}
	if b.beforeSeq != 0 {
		beforeTime = seqTimeNs(d, b.beforeSeq)
	}
	for _, c := range d.Crashes() {
		if c == nil || !crashPkgMatch(c, pkg, own, su) {
			continue
		}
		if !crashInBounds(c, b, afterTime, beforeTime) {
			continue
		}
		if c.Kind == "anr" {
			anrs++
		} else {
			crashes++
		}
	}
	return crashes, anrs, true
}

// crashInBounds applies the same window to every kind (java/native/anr).
func crashInBounds(c *analysis.Crash, b packageCrashBounds, afterTime, beforeTime int64) bool {
	if b.fromNs != 0 && c.TimeNs < b.fromNs {
		return false
	}
	if b.toNs != 0 && c.TimeNs >= b.toNs {
		return false
	}
	if b.afterSeq != 0 {
		switch {
		case c.Seq != 0:
			if c.Seq <= b.afterSeq {
				return false
			}
		case afterTime != 0:
			if c.TimeNs <= afterTime {
				return false
			}
		default:
			return false
		}
	}
	if b.beforeSeq != 0 {
		switch {
		case c.Seq != 0:
			if c.Seq >= b.beforeSeq {
				return false
			}
		case beforeTime != 0:
			if c.TimeNs >= beforeTime {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// seqTimeNs is the device time of the stored record at seq (0 if missing).
func seqTimeNs(d *device.Device, seq uint64) int64 {
	if d == nil || d.Store == nil || seq == 0 {
		return 0
	}
	var t int64
	d.Store.Scan(seq-1, seq+1, false, func(r *model.Record) bool {
		if r.Seq == seq {
			t = r.TimeNs
			return false
		}
		return true
	})
	return t
}

// packageCrashAttach builds header KV (and the optional next: note) for a
// package-scoped MCP ok/empty response. withPackage adds package=<pkg>.
func packageCrashAttach(d *device.Device, pkg string, withPackage bool, b packageCrashBounds) (kv []any, note string) {
	if withPackage {
		kv = append(kv, "package", pkg)
	}
	crashes, anrs, ok := packageCrashCounts(d, pkg, b)
	if !ok {
		return kv, ""
	}
	kv = append(kv, "crashes", crashes, "anrs", anrs)
	if crashes+anrs > 0 {
		next := "logcat_crashes " + param("package", pkg)
		if !b.empty() {
			loc := d.Loc()
			if b.fromNs != 0 {
				next += " " + param("from", isoNs(b.fromNs, loc))
			}
			if b.toNs != 0 {
				next += " " + param("to", isoNs(b.toNs, loc))
			}
		}
		note = hdr("note", "code", "package_crash_counts", "crashes", crashes, "anrs", anrs) +
			nextClause(next) + "\n"
	}
	return kv, note
}

// packageCrashKV is header KV only. Prefer packageCrashAttach when the note
// is also needed.
func packageCrashKV(d *device.Device, pkg string, withPackage bool, b packageCrashBounds) []any {
	kv, _ := packageCrashAttach(d, pkg, withPackage, b)
	return kv
}
