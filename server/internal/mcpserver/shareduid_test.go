package mcpserver

// Issue #44: package= on a shared uid includes the uid's shared processes
// (android.process.acore) with a header note; issue #70: such processes are
// never attributed to the framework package "android".

import (
	"fmt"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

const sharedPackages = `package:com.android.providers.contacts uid:10054
package:com.android.contacts uid:10054
package:com.android.calllogbackup uid:10054
package:com.foo uid:10201
package:android uid:1000
`

func TestPackageSharedUID(t *testing.T) {
	t.Parallel()
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
	ps := []struct {
		pid  int
		uid  int
		name string
	}{
		{2100, 10054, "android.process.acore"},
		{2200, 10054, "com.android.contacts"},
		{2300, 10054, "com.android.providers.contacts"},
		{2400, 10201, "com.foo"},
	}
	var lines []string
	for i, p := range ps {
		lines = append(lines, fxLine(t0+float64(i), 1000, 1500, "I", "am_proc_start",
			fmt.Sprintf("[0,%d,%d,%s,service,{%s/.Svc}]", p.pid, p.uid, p.name, p.name)))
	}
	for i, p := range ps {
		lines = append(lines, fxLine(t0+10+float64(i), p.uid, p.pid, "I", "Attr", "line from "+p.name))
	}
	lines = append(lines,
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "FATAL EXCEPTION: main"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "Process: android.process.acore, PID: 2100"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "java.lang.IllegalStateException: acore broke"),
		fxLine(t0+20, 10054, 2100, "E", "AndroidRuntime", "\tat com.android.providers.contacts.Foo.run(Foo.java:1)"),
		fxLine(t0+20.05, 1000, 1500, "I", "am_crash", "[2100,0,android.process.acore,1,java.lang.IllegalStateException,acore broke,Foo.java,1]"),
	)
	e.replay(strings.Join(lines, "\n"))
	// a crash stored by an older version with package "android" (issue #70)
	e.pipe.EmitCrash(&analysis.Crash{Kind: "java", Boot: fxBoot1, TimeNs: int64((t0 + 15) * 1e9), PID: 2500,
		Process: "android.process.media", Package: "android", Headline: "java.lang.RuntimeException: media broke", Sources: []string{"logcat"}})
	st.Flush()
	if got := procOf(e.pipe, 2100); got != "" {
		t.Errorf("acore attributed to %q, want none", got)
	}

	// each package of the uid: its own process and the shared one, with the note
	note := "note code=shares_uid package=" // logcat_crashes, status
	out := e.ok("logcat_search", `{"package":"com.android.providers.contacts","filter":"tag=:Attr"}`,
		" matched=2 ", "2100/", "2300/", "note code=shares_uid package=com.android.providers.contacts uid=10054 shares_uid=2 next: verbose=true")
	notContains(t, out, "2200/", "2400/", "calllogbackup")
	dump(t, out)
	// verbose lists the packages and the shared processes
	e.ok("logcat_search", `{"package":"com.android.providers.contacts","filter":"tag=:Attr","verbose":true}`,
		"with=com.android.calllogbackup,com.android.contacts shared_processes=android.process.acore")
	out = e.ok("logcat_search", `{"package":"com.android.contacts","filter":"tag=:Attr"}`, " matched=2 ", "2100/", "2200/", "note code=shares_uid package=com.android.contacts")
	notContains(t, out, "2300/")
	// not on a shared uid: no note, no shared lines
	out = e.ok("logcat_search", `{"package":"com.foo","filter":"tag=:Attr"}`, " matched=1 ", "2400/")
	notContains(t, out, "code=shares_uid", "2100/")
	// the package: filter keeps its Android Studio meaning (name prefix)
	out = e.ok("logcat_search", `{"filter":"tag=:Attr package:com.android.providers.contacts"}`, "2300/")
	notContains(t, out, "2100/", "code=shares_uid")

	// crashes: the acore crash is listed for the uid's packages, never for android
	out = e.ok("logcat_crashes", `{"package":"com.android.contacts"}`, "acore broke", note)
	notContains(t, out, "package=android ")
	dump(t, out)
	e.ok("logcat_crashes", `{"package":"android"}`, "empty view=list", "hint code=no_crash_for_package")
	out = e.ok("logcat_crashes", `{"package":"android"}`, "empty view=list", "hint code=no_crash_for_package")
	e.ok("logcat_crashes", `{"kinds":["java"]}`, "media broke", "acore broke")
	for _, c := range device.CrashSummaries(st) {
		if c.Process == "android.process.media" {
			out = e.ok("logcat_crashes", `{"id":"`+c.ID+`"}`, "media broke")
			notContains(t, out, "package=android\n")
		}
	}

	// status lists the shared process with the note
	out = e.ok("logcat_status", `{"package":"com.android.providers.contacts"}`, "android.process.acore", note)
	dump(t, out)
}
