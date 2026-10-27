package mcpserver

import (
	"fmt"
	"strings"
	"testing"
)

// Issue #269: package-scoped MCP ok/empty headers carry store-known
// crashes=/anrs= (crashPkgMatch), including zeros; filter-only package: does not.

func TestPackageCrashCountsZero(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"package":"com.missing","limit":5}`)
	must(t, out, e, "package=com.missing", " crashes=0 ", " anrs=0 ")
	notContains(t, out, "note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=0") || !strings.Contains(head, "anrs=0") {
		t.Errorf("counts must be on header:\n%s", head)
	}
}

func TestPackageCrashCountsSearchLines(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"package":"com.foo","min_level":"error"}`)
	must(t, out, e, "package=com.foo", " crashes=1 ", " anrs=0 ",
		"note code=package_crash_counts crashes=1 anrs=0 next: logcat_crashes package=\"com.foo\"")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") || !strings.Contains(head, "anrs=0") {
		t.Errorf("counts on header:\n%s", head)
	}
}

func TestPackageCrashCountsSearchSummary(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"view":"summary","package":"com.foo"}`)
	must(t, out, e, "view=summary", "package=com.foo", " crashes=1 ", " anrs=0 ")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("summary header counts:\n%s", head)
	}
	// no legacy body line (counts are header-only now)
	if strings.Contains(out, "\ncrashes=1 anrs=0") {
		t.Errorf("legacy body crashes= line still present:\n%s", out)
	}
}

func TestPackageCrashCountsNoPackageParam(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"filter":"tag:Noise","limit":5}`)
	must(t, out, e, "Noise")
	head := firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("no package= => no crash counts on lines header:\n%s", head)
	}
}

func TestPackageCrashCountsFilterOnly(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"filter":"package:com.foo","limit":5}`)
	must(t, out, e)
	head := firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("filter-only package: must not attach counts:\n%s", head)
	}
	notContains(t, out, "note code=package_crash_counts")
}

func TestPackageCrashCountsStatus(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_status", `{"package":"com.foo"}`)
	must(t, out, e, "view=status", " crashes=1 ", " anrs=0 ",
		"note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") || !strings.Contains(head, "anrs=0") {
		t.Errorf("status header counts:\n%s", head)
	}
	if !strings.Contains(out, "package id=com.foo") || !strings.Contains(out, " crashes=1 ") {
		t.Errorf("package fact line should carry counts:\n%s", out)
	}
}

func TestPackageCrashCountsLaunches(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_app_launches", `{"package":"com.foo"}`)
	must(t, out, e, "view=launches", "package=com.foo", " crashes=1 ", " anrs=0 ")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("launches header counts:\n%s", head)
	}
}

func TestPackageCrashCountsExport(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 50)
	out := fx.ok(t, `{"package":"com.missing"}`)
	head := firstLine(out)
	if !strings.Contains(head, "package=com.missing") || !strings.Contains(head, "crashes=0") || !strings.Contains(head, "anrs=0") {
		t.Errorf("export MCP ok header package counts:\n%s", head)
	}
}

func TestPackageCrashCountsErrorUnchanged(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, isErr := call(t, tl, "logcat_search", `{"package":"com.foo","filter":"tag:Net foo:bar"}`)
	if !isErr || !strings.HasPrefix(out, "error code=") {
		t.Fatalf("want error, got isErr=%v:\n%s", isErr, out)
	}
	if strings.Contains(out, "crashes=") || strings.Contains(out, "anrs=") {
		t.Errorf("error path must not grow counts:\n%s", out)
	}
}

func TestPackageCrashCountsReplayJavaAndANR(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	e.file("anr_broadcast.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))

	javaN, anrN := 0, 0
	for _, line := range pkgCrashes(e) {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		switch parts[1] {
		case "anr":
			anrN++
		case "java", "native":
			javaN++
		}
	}
	if javaN < 1 || anrN < 1 {
		t.Fatalf("fixture setup: want >=1 java and >=1 anr, got crashes=%d anrs=%d from %v", javaN, anrN, pkgCrashes(e))
	}
	wantC := fmt.Sprintf(" crashes=%d ", javaN)
	wantA := fmt.Sprintf(" anrs=%d ", anrN)
	out := e.ok("logcat_search", `{"package":"com.example.logtest","limit":3}`,
		"package=com.example.logtest", wantC, wantA, "note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, fmt.Sprintf("crashes=%d", javaN)) || !strings.Contains(head, fmt.Sprintf("anrs=%d", anrN)) {
		t.Errorf("search lines header: want crashes=%d anrs=%d in:\n%s", javaN, anrN, head)
	}
	e.ok("logcat_search", `{"view":"summary","package":"com.example.logtest"}`, wantC, wantA)
	e.ok("logcat_status", `{"package":"com.example.logtest"}`, wantC, wantA)
	e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 02:50:00"}`, wantC, wantA)
}
