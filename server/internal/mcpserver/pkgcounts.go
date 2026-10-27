package mcpserver

import (
	"fmt"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// packageCrashBounds is the effective time/cursor window for #269 counts
// (intersection of from/to/age and after/before). Zero fields mean open.
// limit / matched-line range / wait do not belong here.
type packageCrashBounds struct {
	fromNs, toNs        int64
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
// package-scoped MCP ok/empty response. withPackage adds package=<list>.
//
// Single package (#269): numeric crashes=/anrs=; zeros omitted; note when any
// non-zero. Multi package (#276): named non-zero only, e.g.
// crashes=com.foo:2,com.bar:1 / anrs=com.baz:1; packages with both zero are
// omitted from both keys; all-zero omits keys and the note. next: points at
// logcat_crashes with the first non-zero package in request order (document).
func packageCrashAttach(d *device.Device, pkgs []string, withPackage bool, b packageCrashBounds) (kv []any, note string) {
	if len(pkgs) == 0 {
		return nil, ""
	}
	if withPackage {
		kv = append(kv, "package", packageHeader(pkgs))
	}
	if len(pkgs) == 1 {
		return packageCrashAttachOne(d, pkgs[0], kv, b)
	}
	return packageCrashAttachMulti(d, pkgs, kv, b)
}

func packageCrashAttachOne(d *device.Device, pkg string, kv []any, b packageCrashBounds) ([]any, string) {
	crashes, anrs, ok := packageCrashCounts(d, pkg, b)
	if !ok {
		return kv, ""
	}
	if crashes != 0 {
		kv = append(kv, "crashes", crashes)
	}
	if anrs != 0 {
		kv = append(kv, "anrs", anrs)
	}
	if crashes+anrs == 0 {
		return kv, ""
	}
	return kv, packageCrashNote(pkg, crashes, anrs, b, d)
}

func packageCrashAttachMulti(d *device.Device, pkgs []string, kv []any, b packageCrashBounds) ([]any, string) {
	var crashParts, anrParts []string
	var nextPkg string
	anyOK := false
	for _, pkg := range pkgs {
		crashes, anrs, ok := packageCrashCounts(d, pkg, b)
		if !ok {
			continue
		}
		anyOK = true
		if crashes != 0 {
			crashParts = append(crashParts, fmt.Sprintf("%s:%d", pkg, crashes))
		}
		if anrs != 0 {
			anrParts = append(anrParts, fmt.Sprintf("%s:%d", pkg, anrs))
		}
		// first non-zero in request order for next: (#276)
		if nextPkg == "" && crashes+anrs > 0 {
			nextPkg = pkg
		}
	}
	if !anyOK {
		return kv, ""
	}
	if len(crashParts) > 0 {
		kv = append(kv, "crashes", strings.Join(crashParts, ","))
	}
	if len(anrParts) > 0 {
		kv = append(kv, "anrs", strings.Join(anrParts, ","))
	}
	if nextPkg == "" {
		return kv, ""
	}
	noteKV := []any{"code", "package_crash_counts"}
	if len(crashParts) > 0 {
		noteKV = append(noteKV, "crashes", strings.Join(crashParts, ","))
	}
	if len(anrParts) > 0 {
		noteKV = append(noteKV, "anrs", strings.Join(anrParts, ","))
	}
	next := crashesNext(nextPkg, b, d)
	return kv, hdr("note", noteKV...) + nextClause(next) + "\n"
}

func crashesNext(pkg string, b packageCrashBounds, d *device.Device) string {
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
	return next
}

// packageCrashNote is the single-package next: note (#269).
func packageCrashNote(pkg string, crashes, anrs int, b packageCrashBounds, d *device.Device) string {
	noteKV := []any{"code", "package_crash_counts"}
	if crashes != 0 {
		noteKV = append(noteKV, "crashes", crashes)
	}
	if anrs != 0 {
		noteKV = append(noteKV, "anrs", anrs)
	}
	return hdr("note", noteKV...) + nextClause(crashesNext(pkg, b, d)) + "\n"
}

// packageCrashKV is header KV only. Prefer packageCrashAttach when the note
// is also needed.
func packageCrashKV(d *device.Device, pkgs []string, withPackage bool, b packageCrashBounds) []any {
	kv, _ := packageCrashAttach(d, pkgs, withPackage, b)
	return kv
}

// packageCrashKVOne is single-package header count KV without package= (for
// status package fact lines).
func packageCrashKVOne(d *device.Device, pkg string, b packageCrashBounds) []any {
	return packageCrashKV(d, []string{pkg}, false, b)
}
