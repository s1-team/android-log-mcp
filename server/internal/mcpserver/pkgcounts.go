package mcpserver

import (
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// packageCrashCounts returns store-known crash and ANR counts for pkg using
// the same crashPkgMatch + shared-UID rules as logcat_crashes (issue #269).
// Counts ignore from/to/limit. ok is false when counts cannot be computed
// (caller must omit crashes=/anrs= and never fail the primary ok/empty).
func packageCrashCounts(d *device.Device, pkg string) (crashes, anrs int, ok bool) {
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
	for _, c := range d.Crashes() {
		if c == nil || !crashPkgMatch(c, pkg, own, su) {
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

// packageCrashAttach builds header KV (and the optional next: note) for a
// package-scoped MCP ok/empty response. withPackage adds package=<pkg>.
// Counts are computed once.
func packageCrashAttach(d *device.Device, pkg string, withPackage bool) (kv []any, note string) {
	if withPackage {
		kv = append(kv, "package", pkg)
	}
	crashes, anrs, ok := packageCrashCounts(d, pkg)
	if !ok {
		return kv, ""
	}
	kv = append(kv, "crashes", crashes, "anrs", anrs)
	if crashes+anrs > 0 {
		note = hdr("note", "code", "package_crash_counts", "crashes", crashes, "anrs", anrs) +
			nextClause("logcat_crashes "+param("package", pkg)) + "\n"
	}
	return kv, note
}

// packageCrashKV is header KV only (for call sites that already have the note
// or need counts on a second line without recomputing via Attach when note is
// handled separately). Prefer packageCrashAttach when both are needed.
func packageCrashKV(d *device.Device, pkg string, withPackage bool) []any {
	kv, _ := packageCrashAttach(d, pkg, withPackage)
	return kv
}
