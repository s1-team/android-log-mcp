package mcpserver

// Issue #41: package= must include device processes whose name extends the
// package with a dot (com.google.android.gms.persistent), also when the
// process table was built before the package list was read.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// shared uid 10115 (gms + gsf) as in the report; com.foo next to com.foobar
const attrPackages = `package:com.google.android.gms uid:10115
package:com.google.android.gsf uid:10115
package:com.android.providers.downloads uid:10056
package:com.android.providers.downloads.ui uid:10056
package:com.foo uid:10201
package:com.foobar uid:10202
package:android uid:1000
`

// fxLine formats a line like the captures (`logcat -v epoch,uid`).
func fxLine(sec float64, uid, pid int, lvl, tag, msg string) string {
	return fmt.Sprintf("         %.6f %5d %5d %5d %s %s: %s", sec, uid, pid, pid, lvl, tag, msg)
}

func TestPackageIncludesDottedProcesses(t *testing.T) {
	// like newReplay, but the store has no package list yet (first session
	// on a device): the failing order of issue #41
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

	const t0 = 1791572000.0
	procsAt := []struct {
		pid  int
		uid  int
		name string
	}{
		{3187, 10115, "com.google.android.gms.persistent"},
		{3278, 10115, "com.google.android.gms"},
		{5443, 10115, "com.google.android.gms.unstable"},
		{4100, 10056, "com.android.providers.downloads.ui"},
		{4200, 10202, "com.foobar"},
	}
	var before []string
	for i, p := range procsAt {
		before = append(before, fxLine(t0+float64(i), 1000, 1500, "I", "am_proc_start",
			fmt.Sprintf("[0,%d,%d,%s,service,{%s/.Svc}]", p.pid, p.uid, p.name, p.name)))
	}
	for i, p := range procsAt {
		before = append(before, fxLine(t0+10+float64(i), p.uid, p.pid, "I", "Attr", "before the package list from "+p.name))
	}
	e.replay(strings.Join(before, "\n"))
	if got := procOf(e.pipe, 3187); got != "com.google.android.gms.persistent" {
		t.Fatalf("precondition: before the package list, gms.persistent is attributed to %q", got)
	}
	// a line accepted before the list arrives but stored after it
	queued := &model.Record{TimeNs: int64((t0 + 20) * 1e9), PID: 5443, TID: 5443, UID: 10115, Level: 'I', Tag: "Attr", Msg: "queued while the package list was read"}
	e.pipe.Accept(queued, time.Unix(0, queued.TimeNs))

	pk := procs.ParsePackagesU(attrPackages)
	e.pipe.Procs.SetPackages(pk) // what refreshPackages does
	device.WritePackages(st, pk)
	if err := e.pipe.Drain(true, time.Unix(0, queued.TimeNs).Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := procOf(e.pipe, 3187); got != "com.google.android.gms" {
		t.Errorf("after SetPackages, gms.persistent is attributed to %q", got)
	}

	const t1 = t0 + 60
	var after []string
	for i, p := range procsAt {
		after = append(after, fxLine(t1+float64(i), p.uid, p.pid, "I", "Attr", "after the package list from "+p.name))
	}
	crash := []string{
		fxLine(t1+10, 10115, 3187, "E", "AndroidRuntime", "FATAL EXCEPTION: main"),
		fxLine(t1+10, 10115, 3187, "E", "AndroidRuntime", "Process: com.google.android.gms.persistent, PID: 3187"),
		fxLine(t1+10, 10115, 3187, "E", "AndroidRuntime", "java.lang.IllegalStateException: persistent broke"),
		fxLine(t1+10, 10115, 3187, "E", "AndroidRuntime", "\tat com.google.android.gms.Foo.run(Foo.java:1)"),
		fxLine(t1+10.05, 1000, 1500, "I", "am_crash", "[3187,0,com.google.android.gms.persistent,1,java.lang.IllegalStateException,persistent broke,Foo.java,1]"),
	}
	e.replay(strings.Join(append(after, crash...), "\n"))

	loc, _ := device.ResolveZone("Asia/Kuching", "+0800", fxNow())
	from := time.Unix(int64(t0+19), 0).In(loc).Format("2006-01-02 15:04:05")

	// all three gms device processes, the queued line included
	out := e.ok("logcat_search", `{"package":"com.google.android.gms","filter":"tag=:Attr","from":"`+from+`"}`,
		"matched 4 lines", "3187/", "3278/", "5443/", "queued while the package list was read")
	notContains(t, out, "4100/", "4200/")
	dump(t, out)
	// an installed package that extends another one keeps its own lines
	out = e.ok("logcat_search", `{"package":"com.android.providers.downloads","filter":"tag=:Attr"}`)
	notContains(t, out, "4100/")
	e.ok("logcat_search", `{"package":"com.android.providers.downloads.ui","filter":"tag=:Attr","from":"`+from+`"}`, "matched 1 line", "4100/")
	// negatives: no prefix match on package ids
	out = e.ok("logcat_search", `{"package":"com.google","from":"`+from+`"}`, "matched 0 lines", "com.google.android.gms")
	notContains(t, out, "com.google.android.gms.persistent", "com.google.android.gms.unstable")
	dump(t, out)
	out = e.ok("logcat_search", `{"package":"com.foo","filter":"tag=:Attr"}`)
	notContains(t, out, "4200/")
	// crashes raised in the dotted device process
	out = e.ok("logcat_crashes", `{"package":"com.google.android.gms"}`, "persistent broke")
	dump(t, out)
	// the re-attributed entry was persisted (procs.jsonl)
	found := false
	for _, p := range device.LoadProcs(e.st) {
		if p.PID == 3187 {
			found = p.Package == "com.google.android.gms"
		}
	}
	if !found {
		t.Errorf("procs.jsonl still has the fallback package for gms.persistent")
	}
}

func procOf(p *device.Pipeline, pid int) string {
	r := &model.Record{PID: pid, TimeNs: 1 << 62, UID: -1}
	p.Procs.Annotate(r)
	return r.Package
}
