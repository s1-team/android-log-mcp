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

// packageCrashKV returns header KV for package-scoped MCP ok/empty responses.
// withPackage adds package=<pkg>. When counts are unavailable, only package=
// is returned (when withPackage) so primary responses stay ok.
func packageCrashKV(d *device.Device, pkg string, withPackage bool) []any {
	var kv []any
	if withPackage {
		kv = append(kv, "package", pkg)
	}
	crashes, anrs, ok := packageCrashCounts(d, pkg)
	if !ok {
		return kv
	}
	return append(kv, "crashes", crashes, "anrs", anrs)
}

// packageCrashNote is the optional body line when crashes+anrs > 0, pointing
// agents at logcat_crashes for the same package (store-known, no from/to).
func packageCrashNote(d *device.Device, pkg string) string {
	crashes, anrs, ok := packageCrashCounts(d, pkg)
	if !ok || crashes+anrs == 0 {
		return ""
	}
	return hdr("note", "code", "package_crash_counts", "crashes", crashes, "anrs", anrs) +
		nextClause("logcat_crashes "+param("package", pkg)) + "\n"
}
