package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Issue #276: multi-package structured package= + named per-package crash/ANR counts.

func TestPackageListUnmarshal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{`null`, nil},
		{`"com.foo"`, []string{"com.foo"}},
		{`"  com.foo  "`, []string{"com.foo"}},
		{`"com.foo,com.bar"`, []string{"com.foo", "com.bar"}},
		{`"com.foo, com.bar , com.baz"`, []string{"com.foo", "com.bar", "com.baz"}},
		{`"com.foo,com.foo,com.bar"`, []string{"com.foo", "com.bar"}},
		{`["com.foo","com.bar"]`, []string{"com.foo", "com.bar"}},
		{`[" com.foo ","","com.bar","com.foo"]`, []string{"com.foo", "com.bar"}},
		{`[]`, nil},
	}
	for _, c := range cases {
		var p PackageList
		if err := json.Unmarshal([]byte(c.in), &p); err != nil {
			t.Fatalf("unmarshal %s: %v", c.in, err)
		}
		if len(p) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.in, []string(p), c.want)
		}
		for i := range c.want {
			if p[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", c.in, []string(p), c.want)
			}
		}
	}
}

func TestMultiPackageSearchNamedCounts(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	// array: com.foo has 1 crash, com.missing has 0 → named crashes for foo only
	out, e := call(t, tl, "logcat_search", `{"package":["com.foo","com.missing"],"min_level":"error"}`)
	must(t, out, e, "package=com.foo,com.missing", " crashes=com.foo:1 ",
		`note code=package_crash_counts crashes=com.foo:1 next: logcat_crashes package="com.foo"`)
	head := firstLine(out)
	if strings.Contains(head, "anrs=") {
		t.Errorf("zero anrs must be omitted:\n%s", head)
	}
	if strings.Contains(head, "crashes=1 ") || strings.Contains(head, " crashes=1") {
		t.Errorf("multi must use named counts, not bare crashes=1:\n%s", head)
	}
	// comma-separated string compat
	out, e = call(t, tl, "logcat_search", `{"package":"com.missing,com.foo","limit":5}`)
	must(t, out, e, "package=com.missing,com.foo", " crashes=com.foo:1 ",
		`next: logcat_crashes package="com.foo"`) // first non-zero in request order = com.foo
	// request order: missing first still next=com.foo (only non-zero)
	out, e = call(t, tl, "logcat_search", `{"package":["com.missing","com.foo"],"limit":1}`)
	must(t, out, e, "package=com.missing,com.foo", `next: logcat_crashes package="com.foo"`)
}

func TestMultiPackageAllZeroOmits(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"package":["com.missing","com.also"],"limit":5}`)
	must(t, out, e, "package=com.missing,com.also")
	notContains(t, out, "note code=package_crash_counts")
	head := firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("all-zero multi must omit count keys:\n%s", head)
	}
}

func TestMultiPackageCap(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf("com.p%d", i)
	}
	b, _ := json.Marshal(map[string]any{"package": ids, "limit": 1})
	out, isErr := call(t, tl, "logcat_search", string(b))
	if !isErr || !strings.Contains(out, "bad_param") || !strings.Contains(out, "package") {
		t.Fatalf("want bad_param for >20 packages, got isErr=%v:\n%s", isErr, out)
	}
	if !strings.Contains(out, "max 20") && !strings.Contains(out, "valid=max 20") {
		t.Errorf("want max 20 in error:\n%s", out)
	}
}

func TestMultiPackageFilterOnlyNoCounts(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"filter":"package:com.foo | package:com.missing","limit":5}`)
	must(t, out, e)
	head := firstLine(out)
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("filter-only multi must not invent counts:\n%s", head)
	}
	notContains(t, out, "note code=package_crash_counts")
}

func TestMultiPackageORMatch(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	// com.foo lines match; searching both should still see Net/Repo/crash lines from foo
	out, e := call(t, tl, "logcat_search", `{"package":["com.missing","com.foo"],"filter":"tag=:Net","limit":5}`)
	must(t, out, e, "package=com.missing,com.foo", "Net:")
	if !strings.Contains(out, "request") {
		t.Errorf("OR match should include com.foo Net lines:\n%s", out)
	}
}

func TestMultiPackageStatusLaunchesExport(t *testing.T) {
	t.Parallel()
	tl := fixture(t)
	out, e := call(t, tl, "logcat_status", `{"package":["com.foo","com.missing"]}`)
	must(t, out, e, "package=com.foo,com.missing", " crashes=com.foo:1 ",
		"package id=com.foo", "note code=package_crash_counts")
	if !strings.Contains(out, "package id=com.missing") {
		t.Errorf("status should emit a fact line per requested package:\n%s", out)
	}

	out, e = call(t, tl, "logcat_app_launches", `{"package":["com.foo","com.missing"]}`)
	must(t, out, e, "view=launches", "package=com.foo,com.missing", " crashes=com.foo:1 ")

	fx := newExFx(t, 50)
	out = fx.ok(t, `{"package":["com.missing","com.also"]}`)
	head := firstLine(out)
	if !strings.Contains(head, "package=com.missing,com.also") {
		t.Errorf("export MCP ok header package list:\n%s", head)
	}
	if strings.Contains(head, "crashes=") || strings.Contains(head, "anrs=") {
		t.Errorf("zero multi counts must be omitted from export:\n%s", head)
	}
}

func TestMultiPackageTwoNonZeroNamed(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	// second package with its own java crash
	e.pipe.EmitCrash(&analysis.Crash{
		Kind: "java", Boot: fxBoot1, TimeNs: int64(1.7e18), PID: 9999,
		Process: "com.other", Package: "com.other",
		Headline: "java.lang.RuntimeException: other broke", Sources: []string{"logcat"},
	})
	e.st.Flush()

	out := e.ok("logcat_search", `{"package":["com.other","com.example.logtest"],"limit":1}`,
		"package=com.other,com.example.logtest",
		"note code=package_crash_counts")
	head := firstLine(out)
	if !strings.Contains(head, "crashes=com.other:1,com.example.logtest:1") &&
		!strings.Contains(head, "crashes=com.example.logtest:1,com.other:1") {
		// must be request order: com.other first
		if !strings.Contains(head, "crashes=com.other:1,com.example.logtest:1") {
			t.Errorf("want named crashes in request order:\n%s", head)
		}
	}
	// next: first non-zero in request order = com.other
	if !strings.Contains(out, `next: logcat_crashes package="com.other"`) {
		t.Errorf("next should be first non-zero (com.other):\n%s", out)
	}
	// reverse request order → next is logtest
	out = e.ok("logcat_search", `{"package":["com.example.logtest","com.other"],"limit":1}`)
	if !strings.Contains(out, `next: logcat_crashes package="com.example.logtest"`) {
		t.Errorf("next should follow request order:\n%s", out)
	}
	if !strings.Contains(firstLine(out), "crashes=com.example.logtest:1,com.other:1") {
		t.Errorf("named crashes request order:\n%s", firstLine(out))
	}
}

func TestMultiPackageSharedUIDCounts(t *testing.T) {
	t.Parallel()
	// Shared-UID crash (acore) counts for contacts under multi package= (#276).
	e := &replayEnv{t: t, base: replayDir(t)}
	st, err := store.Open(e.base, fxSerial, store.Config{})
	if err != nil || !st.IsWriter() {
		t.Fatalf("open: %v", err)
	}
	e.st = st
	t.Cleanup(st.Close)
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.Model, m.Release, m.SDK = fxSerial, "Pixel 4", "12", "32"
		m.TZ, m.TZOffset = "Asia/Kuching", "+0800"
	})
	e.newSession(fxBoot1)
	pk := procs.ParsePackagesU(sharedPackages)
	e.pipe.Procs.SetPackages(pk)
	device.WritePackages(st, pk)
	const t0 = 1791572000.0
	lines := []string{
		fxLine(t0, 1000, 1500, "I", "am_proc_start", "[0,2100,10054,android.process.acore,service,{android.process.acore/.Svc}]"),
		fxLine(t0+1, 1000, 1500, "I", "am_proc_start", "[0,2400,10201,com.foo,service,{com.foo/.Svc}]"),
		fxLine(t0+10, 10054, 2100, "I", "Attr", "line from acore"),
		fxLine(t0+11, 10201, 2400, "I", "Attr", "line from foo"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "FATAL EXCEPTION: main"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "Process: android.process.acore, PID: 2100"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "java.lang.IllegalStateException: acore broke"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "\tat com.android.providers.contacts.Foo.run(Foo.java:1)"),
		fxLine(t0+20.05, 1000, 1500, "I", "am_crash", "[2100,0,android.process.acore,1,java.lang.IllegalStateException,acore broke,Foo.java,1]"),
	}
	e.replay(strings.Join(lines, "\n"))
	st.Flush()
	out := e.ok("logcat_search", `{"package":["com.android.contacts","com.foo"],"limit":5}`,
		"package=com.android.contacts,com.foo",
		" crashes=com.android.contacts:1 ",
		"note code=shares_uid package=com.android.contacts")
	head := firstLine(out)
	if strings.Contains(head, "crashes=com.foo:") {
		t.Errorf("com.foo has no crash; must not appear in named crashes=:\n%s", head)
	}
	if !strings.Contains(out, "2100/") || !strings.Contains(out, "2400/") {
		t.Errorf("OR match should include both packages' lines:\n%s", out)
	}
}
