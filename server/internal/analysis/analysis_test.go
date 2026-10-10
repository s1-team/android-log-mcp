package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// feed parses raw logcat text through the real assembler.
func records(t *testing.T, raw string) []*model.Record {
	t.Helper()
	a := &logcat.Assembler{}
	var out []*model.Record
	seq := uint64(0)
	add := func(r *model.Record) {
		if r == nil {
			return
		}
		seq++
		r.Seq, r.Boot = seq, "b1"
		if r.UID == 10123 {
			r.Package = "com.foo"
			r.Process = "com.foo"
		}
		out = append(out, r)
	}
	for _, ln := range strings.Split(strings.TrimSpace(raw), "\n") {
		add(a.Push(strings.TrimLeft(ln, "\t")))
	}
	add(a.Flush())
	return out
}

const crashLog = `--------- beginning of crash
1791570400.100000 10123  4321  4321 E AndroidRuntime: FATAL EXCEPTION: main
1791570400.100000 10123  4321  4321 E AndroidRuntime: Process: com.foo, PID: 4321
1791570400.100000 10123  4321  4321 E AndroidRuntime: java.lang.RuntimeException: Unable to start activity
1791570400.100000 10123  4321  4321 E AndroidRuntime: 	at android.app.ActivityThread.performLaunchActivity(ActivityThread.java:3449)
1791570400.100000 10123  4321  4321 E AndroidRuntime: Caused by: java.lang.IllegalStateException: boom
1791570400.100000 10123  4321  4321 E AndroidRuntime: 	at com.foo.Main.onCreate(Main.kt:10)
--------- beginning of events
1791570400.200000  1000  1880  2000 I am_crash: [0,4321,com.foo,952745540,java.lang.IllegalStateException,boom,Main.kt,10,0]
--------- switch to crash
1791570410.000000 10123  5000  5010 F libc    : Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid 5010 (RenderThread), pid 5000 (com.foo)
1791570410.300000  1000  5100  5100 F DEBUG   : *** *** *** *** *** *** *** *** *** *** *** *** *** *** *** ***
1791570410.300100  1000  5100  5100 F DEBUG   : Build fingerprint: 'google/sdk/x:15/AP/1:user/release-keys'
1791570410.300200  1000  5100  5100 F DEBUG   : pid: 5000, tid: 5010, name: RenderThread  >>> com.foo <<<
1791570410.300300  1000  5100  5100 F DEBUG   : signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000
1791570410.300400  1000  5100  5100 F DEBUG   : backtrace:
1791570410.300500  1000  5100  5100 F DEBUG   :       #00 pc 000000000001a2b4  /data/app/~~x/com.foo-1/lib/arm64/libfoo.so (Java_com_foo_Native_decode+44) (BuildId: abc)
--------- switch to events
1791570420.000000  1000  1880  2001 I am_anr  : [0,6000,com.foo,952745540,Input dispatching timed out (com.foo/.Main, Waited 5001ms for KeyEvent)]
--------- switch to system
1791570420.500000  1000  1880  2001 E ActivityManager: ANR in com.foo (com.foo/.Main)
1791570420.500000  1000  1880  2001 E ActivityManager: PID: 6000
1791570420.500000  1000  1880  2001 E ActivityManager: Reason: Input dispatching timed out (com.foo/.Main, Waited 5001ms for KeyEvent)
1791570420.500000  1000  1880  2001 E ActivityManager: CPU usage from 0ms to 5000ms later:
1791570420.500000  1000  1880  2001 E ActivityManager:   50% 6000/com.foo: 40% user + 10% kernel
1791570430.000000  1000  1880  2001 I ActivityManager: unrelated`

func TestDetector(t *testing.T) {
	d := NewDetector()
	var got []*Crash
	var tags []string
	uniq := map[*Crash]bool{}
	d.Emit = func(c *Crash) { cp := *c; got = append(got, &cp); uniq[c] = true }
	d.NeedDropbox = func(tag string) { tags = append(tags, tag) }
	for _, r := range records(t, crashLog) {
		d.Feed(r)
	}
	d.Tick(1791570500e9)
	byKind := map[string]*Crash{}
	for _, c := range got {
		byKind[c.Kind] = c // last version wins
	}
	j := byKind["java"]
	if j == nil || j.PID != 4321 || j.Thread != "main" || j.Headline != "java.lang.RuntimeException: Unable to start activity" {
		t.Fatalf("java: %+v", j)
	}
	if j.Signature != "java.lang.IllegalStateException@com.foo.Main.onCreate" {
		t.Errorf("sig %q", j.Signature)
	}
	nj := 0
	for c := range uniq {
		if c.Kind == "java" {
			nj++
		}
	}
	if nj != 1 {
		t.Errorf("am_crash should be deduped, got %d java crashes", nj)
	}
	n := byKind["native"]
	if n == nil || n.PID != 5000 || !strings.Contains(n.Headline, "SIGSEGV (SEGV_MAPERR)") || !strings.Contains(n.Headline, "libfoo.so (Java_com_foo_Native_decode+44)") {
		t.Fatalf("native: %+v", n)
	}
	if !strings.Contains(n.Body, ">>> com.foo <<<") || n.Package != "com.foo" {
		t.Errorf("native body/package: %+v", n)
	}
	a := byKind["anr"]
	if a == nil || a.PID != 6000 || !strings.HasPrefix(a.Headline, "Input dispatching timed out") || !strings.Contains(a.CPU, "CPU usage") {
		t.Fatalf("anr: %+v", a)
	}
	if len(a.Sources) != 2 {
		t.Errorf("anr sources %v", a.Sources)
	}
	if strings.Join(tags, ",") == "" {
		t.Error("dropbox should be requested")
	}
}

const dropboxOut = `Drop box contents: 3 entries
Max entries: 1000
Low priority rate limit period: 2000 ms
Low priority tags: {}
Searching for: data_app_anr

========================================
2026-10-10 02:40:20 data_app_anr (text, 2048 bytes)
Process: com.foo
PID: 6000
UID: 10123
Subject: Input dispatching timed out (com.foo/.Main, Waited 5001ms for KeyEvent)

CPU usage from 0ms to 5000ms later:
  50% 6000/com.foo

----- pid 6000 at 2026-10-10 02:40:20.123 -----
Cmd line: com.foo

"main" prio=5 tid=1 Blocked
  | group="main" sCount=1
  at com.foo.db.Cache.get(Cache.kt:55)
  - waiting to lock <0x0abc> (a java.lang.Object) held by thread 23

"DiskIO" prio=5 tid=23 Native
  at com.foo.db.Cache.load(Cache.kt:99)

========================================
2026-10-09 10:00:00 data_app_anr (text, 100 bytes)
Process: com.old
PID: 99
Subject: Broadcast of Intent { act=X }

----- pid 99 at 2026-10-09 10:00:00 -----
"main" prio=5 tid=1 Native
  at android.os.MessageQueue.nativePollOnce(Native Method)
`

func TestDropbox(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	es := ParseDropbox(dropboxOut, loc)
	if len(es) != 2 || es[0].Tag != "data_app_anr" {
		t.Fatalf("%d entries %+v", len(es), es)
	}
	if time.Unix(0, es[0].TimeNs).In(loc).Format("15:04:05") != "02:40:20" {
		t.Errorf("time")
	}
	// existing ANR at 2026-10-10 02:40:20 +08 = 1791571220
	anr := &Crash{ID: "anr1", Kind: "anr", PID: 6000, Process: "com.foo", TimeNs: 1791571220e9 + 5e8}
	c, isNew := MatchDropbox([]*Crash{anr}, es[0])
	if c == nil || isNew || c.ID != "anr1" || !strings.Contains(c.Traces, `"main" prio=5`) {
		t.Fatalf("match: %+v new=%v", c, isNew)
	}
	main, holder := MainThreadTrace(c.Traces)
	if !strings.Contains(main, "Cache.get") || !strings.Contains(holder, `"DiskIO"`) {
		t.Errorf("main=%q holder=%q", main, holder)
	}
	c2, isNew := MatchDropbox([]*Crash{anr}, es[1])
	if !isNew || c2.Process != "com.old" || c2.Headline != "Broadcast of Intent { act=X }" || c2.Traces == "" {
		t.Fatalf("new from dropbox: %+v", c2)
	}
}

func TestDropboxJavaCrash(t *testing.T) {
	out := "========================================\n2026-10-10 02:00:00 data_app_crash (text, 300 bytes)\nProcess: com.bar\nPID: 777\nUID: 10200\nFlags: 0x1\nPackage: com.bar v1 (1.0)\nForeground: Yes\nBuild: x\n\njava.lang.NullPointerException: oops\n\tat com.bar.A.b(A.java:1)\n"
	es := ParseDropbox(out, time.UTC)
	if len(es) != 1 {
		t.Fatalf("%+v", es)
	}
	c, isNew := MatchDropbox(nil, es[0])
	if !isNew || c.Kind != "java" || c.PID != 777 || c.Headline != "java.lang.NullPointerException: oops" || c.Signature != "java.lang.NullPointerException@com.bar.A.b" {
		t.Fatalf("%+v", c)
	}
}

const launchLog = `--------- beginning of system
1791570500.000000  1000  1880  2100 I ActivityTaskManager: START u0 {act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] flg=0x10200000 cmp=com.foo/.ui.MainActivity} with LAUNCH_MULTIPLE from uid 2000 (BAL_ALLOW_PERMISSION) result code=0
--------- switch to events
1791570500.038000  1000  1880  1900 I am_proc_start: [0,23011,10123,com.foo,next-top-activity,{com.foo/com.foo.ui.MainActivity}]
1791570500.412000 10123 23011 23011 I wm_on_create_called: [123,com.foo.ui.MainActivity,performCreate,186]
1791570500.431000 10123 23011 23011 I wm_on_start_called: [123,com.foo.ui.MainActivity,handleStartActivity,9]
1791570500.440000 10123 23011 23011 I wm_on_resume_called: [123,com.foo.ui.MainActivity,RESUME_ACTIVITY,6]
--------- switch to system
1791570500.812000  1000  1880  1920 I ActivityTaskManager: Displayed com.foo/.ui.MainActivity for user 0: +812ms
1791570600.000000  1000  1880  2100 I ActivityTaskManager: START u0 {act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] flg=0x10200000 cmp=com.foo/.ui.MainActivity} from uid 10132
--------- switch to events
1791570600.100000 10123 23011 23011 I wm_on_resume_called: [123,com.foo.ui.MainActivity,RESUME_ACTIVITY]
--------- switch to system
1791570600.300000  1000  1880  1920 I ActivityManager: Displayed com.foo/.ui.MainActivity: +1s20ms`

func TestLaunches(t *testing.T) {
	ls := Launches(records(t, launchLog), "com.foo")
	if len(ls) != 2 {
		t.Fatalf("%d launches", len(ls))
	}
	l := ls[0]
	if l.Kind != "COLD" || l.PID != 23011 || l.OnCreate.TookMs != 186 || l.Displayed.TookMs != 812 {
		t.Fatalf("%+v", l)
	}
	if (l.OnResume.Ns-l.RequestNs)/1e6 != 440 {
		t.Errorf("resume +%d", (l.OnResume.Ns-l.RequestNs)/1e6)
	}
	if ls[1].Kind != "HOT" || ls[1].Displayed.TookMs != 1020 {
		t.Errorf("second %+v", ls[1])
	}
	if Launches(records(t, launchLog), "com.other") != nil {
		t.Error("package filter")
	}
	if ParseAndroidDuration("+1m2s3ms") != 62003 {
		t.Error("dur")
	}
}
