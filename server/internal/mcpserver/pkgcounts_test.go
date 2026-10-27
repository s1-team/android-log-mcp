package mcpserver

import (
	"fmt"
	"strings"
	"testing"
)

// Issue #269: package-scoped MCP ok/empty headers carry store-known
// crashes=/anrs= (crashPkgMatch); zeros are omitted. filter-only package: does not.

func TestPackageCrashCountsZero(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"package":"com.missing","limit":5}`)
	must(t, out, e, "package=com.missing")
	notContains(t, out, "note code=package_crash_counts")
	head := firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("zero counts must be omitted from header:\n%s", head)
	}
}

func TestPackageCrashCountsSearchLines(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"package":"com.foo","min_level":"error"}`)
	must(t, out, e, "package=com.foo", " crashes=1 ",
		"note code=package_crash_counts crashes=1 next: logcat_crashes package=\"com.foo\"")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("counts on header:\n%s", head)
	}
	if strings.Contains(head, "anrs=") {
		t.Errorf("zero anrs= must be omitted:\n%s", head)
	}
	if strings.Contains(out, "anrs=0") {
		t.Errorf("anrs=0 noise:\n%s", out)
	}
}

func TestPackageCrashCountsSearchSummary(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"view":"summary","package":"com.foo"}`)
	must(t, out, e, "view=summary", "package=com.foo", " crashes=1 ")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("summary header counts:\n%s", head)
	}
	if strings.Contains(head, "anrs=") {
		t.Errorf("zero anrs= must be omitted:\n%s", head)
	}
	// no legacy body line (counts are header-only now)
	if strings.Contains(out, "\ncrashes=1 anrs=0") || strings.Contains(out, "\ncrashes=1\n") {
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
	must(t, out, e, "view=status", " crashes=1 ",
		"note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("status header counts:\n%s", head)
	}
	if strings.Contains(head, "anrs=") {
		t.Errorf("zero anrs= must be omitted:\n%s", head)
	}
	if !strings.Contains(out, "package id=com.foo") || !strings.Contains(out, " crashes=1 ") {
		t.Errorf("package fact line should carry counts:\n%s", out)
	}
	if strings.Contains(out, "anrs=0") {
		t.Errorf("anrs=0 noise:\n%s", out)
	}
}

func TestPackageCrashCountsLaunches(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_app_launches", `{"package":"com.foo"}`)
	must(t, out, e, "view=launches", "package=com.foo", " crashes=1 ")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=1") {
		t.Errorf("launches header counts:\n%s", head)
	}
	if strings.Contains(head, "anrs=") {
		t.Errorf("zero anrs= must be omitted:\n%s", head)
	}
}

func TestPackageCrashCountsExport(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 50)
	out := fx.ok(t, `{"package":"com.missing"}`)
	head := firstLine(out)
	if !strings.Contains(head, "package=com.missing") {
		t.Errorf("export MCP ok header package:\n%s", head)
	}
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("zero package counts must be omitted from export MCP ok:\n%s", head)
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
	// launches default from=30m still covers both events on this fixture
	e.ok(launches, `{"package":"com.example.logtest"}`, wantC, wantA)
	e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 02:50:00","to":"2026-10-10 03:10:00"}`, wantC, wantA)
}

func TestPackageCrashCountsRespectsFromTo(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	e.file("anr_broadcast.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))

	// store-known (no from/to): both java + anr
	store := e.ok("logcat_search", `{"package":"com.example.logtest","limit":1}`, "note code=package_crash_counts")
	storeHead := firstLine(store)
	if !strings.Contains(storeHead, "crashes=") || !strings.Contains(storeHead, "anrs=") {
		t.Fatalf("store-known header:\n%s", storeHead)
	}

	// window after the java crash, covering the ANR only (ANR at 03:05:53)
	out := e.ok("logcat_search", `{"package":"com.example.logtest","from":"2026-10-10 03:05:00","to":"2026-10-10 03:06:00","limit":1}`,
		" anrs=1 ")
	head := firstLine(out)
	if !strings.Contains(head, "anrs=1") {
		t.Errorf("windowed header want anrs=1:\n%s", head)
	}
	if strings.Contains(head, "crashes=") {
		t.Errorf("zero crashes= must be omitted:\n%s", head)
	}
	if strings.Contains(storeHead, "anrs=1") && !strings.Contains(storeHead, "crashes=") {
		t.Errorf("store-known unexpectedly matched the narrow window:\n%s", storeHead)
	}

	// window before both events: omit both keys, no next note
	out = e.ok("logcat_search", `{"package":"com.example.logtest","from":"2026-10-10 02:00:00","to":"2026-10-10 02:30:00","limit":1}`)
	notContains(t, out, "note code=package_crash_counts")
	head = firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("both-zero window must omit count keys:\n%s", head)
	}

	// limit does not further narrow: tiny limit still reports windowed counts
	e.ok("logcat_search", `{"package":"com.example.logtest","from":"2026-10-10 03:00:00","to":"2026-10-10 03:10:00","limit":1}`,
		" anrs=1 ")

	// launches with from/to → windowed (same bounds for crashes and anrs)
	out = e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 03:05:00","to":"2026-10-10 03:06:00"}`,
		" anrs=1 ")
	if strings.Contains(firstLine(out), "crashes=") {
		t.Errorf("zero crashes= must be omitted on launches:\n%s", firstLine(out))
	}
}

func TestPackageCrashCountsAgeAndAfter(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	e.file("anr_broadcast.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))

	// age: same effective window as the line scan — java (earlier) excluded, ANR in the last 1m kept
	out := e.ok("logcat_search", `{"package":"com.example.logtest","filter":"age:1m","limit":1}`,
		" anrs=1 ", "note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, "anrs=1") {
		t.Errorf("age window header:\n%s", head)
	}
	if strings.Contains(head, "crashes=") {
		t.Errorf("zero crashes= must be omitted:\n%s", head)
	}

	// after = newest status cursor → nothing newer; both kinds zero → omit keys + note
	st := e.ok("logcat_status", `{}`)
	cur := ""
	for _, part := range strings.Fields(firstLine(st)) {
		if strings.HasPrefix(part, "cursor=") && part != "cursor=none" {
			cur = strings.TrimPrefix(part, "cursor=")
			break
		}
	}
	if cur == "" {
		t.Fatalf("no status cursor in:\n%s", st)
	}
	out = e.ok("logcat_search", fmt.Sprintf(`{"package":"com.example.logtest","after":%q,"limit":1}`, cur))
	notContains(t, out, "note code=package_crash_counts")
	head = firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("after-cursor both-zero must omit count keys:\n%s", head)
	}

	// before = newest cursor → all retained crashes/anrs still in range (intersection open lower)
	e.ok("logcat_search", fmt.Sprintf(`{"package":"com.example.logtest","before":%q,"limit":1}`, cur),
		"note code=package_crash_counts")
}

func TestPackageCrashCountsLimitDoesNotNarrow(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	// fixture has one java crash for com.foo; limit=1 still shows crashes=1
	out, e := call(t, tl, "logcat_search", `{"package":"com.foo","limit":1}`)
	must(t, out, e, " crashes=1 ")
	if strings.Contains(firstLine(out), "anrs=") {
		t.Errorf("zero anrs= must be omitted:\n%s", firstLine(out))
	}
}
