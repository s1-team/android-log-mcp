package mcpserver

// Regression tests that replay real API 29 captures (internal/testdata/api29,
// real API 29 captures, see testdata/api29/README.md) through the production ingest
// pipeline and the MCP tools. Each test names the validation finding it
// guards against (validation/README.md §4).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

const (
	fxSerial = "emulator-5556"
	fxPkg    = "com.example.logtest"
	fxBoot1  = "07a6d2d3" // boot ids of the three sessions in the captures
	fxBoot2  = "bf71fe22"
	fxBoot3  = "ccb368e7"
)

// packages as printed by `pm list packages -U` on the emulator (excerpt)
const fxPackages = `package:com.example.logtest uid:10146
package:com.google.android.apps.nexuslauncher uid:10108
package:com.android.systemui uid:10110
package:com.android.nfc uid:1027
package:android uid:1000
package:com.android.settings uid:1000
package:com.android.providers.media uid:10056
`

type replayEnv struct {
	t     *testing.T
	base  string
	st    *store.Store
	pipe  *device.Pipeline
	boot  string
	tools *Tools
}

func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "api29", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// replayDir is t.TempDir plus a check, run after every later cleanup (the
// store's / Manager's Close), that no file in it is still open. On Windows an
// open handle makes the TempDir removal fail ("being used by another
// process"); /proc/self/fd lets Linux CI catch the same leak.
func replayDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return // not Linux
		}
		for _, e := range ents {
			p, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
			if err == nil && strings.HasPrefix(p, dir+string(os.PathSeparator)) {
				t.Errorf("file still open at end of test: %s", p)
			}
		}
	})
	return dir
}

func newReplay(t *testing.T, boot string) *replayEnv {
	t.Helper()
	e := &replayEnv{t: t, base: replayDir(t)}
	st, err := store.Open(e.base, fxSerial, store.Config{})
	if err != nil || !st.IsWriter() {
		t.Fatalf("open: %v", err)
	}
	e.st = st
	// tests that never call a tool (e.call closes it) still release the
	// segment and writer.lock; Close is idempotent
	t.Cleanup(st.Close)
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.Model, m.Release, m.SDK = fxSerial, "Android SDK built for x86_64", "10", "29"
		m.TZ, m.TZOffset = "Asia/Kuching", "+0800"
	})
	device.WritePackages(st, procs.ParsePackagesU(fxPackages))
	e.newSession(boot)
	return e
}

// newSession starts a boot session exactly like the recorder does.
func (e *replayEnv) newSession(boot string) {
	next := e.st.LastSeq() + 1
	e.st.UpdateMeta(func(m *store.Meta) {
		if m.LastBootID != "" {
			m.Gaps = append(m.Gaps, store.Gap{FromNs: m.LastTimeNs, Reason: "device rebooted"})
		}
		m.Sessions = append(m.Sessions, store.Session{BootID: boot + "-0000", Boot: boot, FirstSeq: next})
		m.LastBootID = boot + "-0000"
	})
	device.CloseOtherBoots(e.st, boot)
	loc, _ := device.ResolveZone("Asia/Kuching", "+0800", fxNow())
	e.pipe = device.NewPipeline(e.st, boot, loc)
	e.boot = boot
}

func (e *replayEnv) replay(text string) {
	e.t.Helper()
	if e.tools != nil {
		e.t.Fatal("replay after tools()")
	}
	if err := e.pipe.ReplayText(text); err != nil {
		e.t.Fatal(err)
	}
}

func (e *replayEnv) file(names ...string) {
	for _, n := range names {
		e.replay(fixtureFile(e.t, n))
	}
}

// splitAt cuts a capture at the first line with epoch >= sec.
func splitAt(text string, sec float64) (string, string) {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		f := strings.Fields(ln)
		if len(f) > 0 && !strings.HasPrefix(f[0], "-") {
			var v float64
			if _, err := fmt.Sscanf(f[0], "%f", &v); err == nil && v >= sec {
				return strings.Join(lines[:i], "\n"), strings.Join(lines[i:], "\n")
			}
		}
	}
	return text, ""
}

func (e *replayEnv) call(name, args string) (string, bool) {
	e.t.Helper()
	if e.tools == nil {
		e.st.Close()
		m := device.NewManager(e.base, store.Config{})
		m.NoADB = true
		m.Start(context.Background())
		e.t.Cleanup(m.Close)
		e.tools = &Tools{M: m}
	}
	return e.tools.Call(context.Background(), name, json.RawMessage(args))
}

func (e *replayEnv) ok(name, args string, want ...string) string {
	e.t.Helper()
	out, isErr := e.call(name, args)
	if isErr {
		e.t.Fatalf("%s %s: error:\n%s", name, args, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			e.t.Errorf("%s %s: missing %q in:\n%s", name, args, w, out)
		}
	}
	return out
}

func notContains(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("unexpected %q in:\n%s", b, out)
		}
	}
}

func dump(t *testing.T, out string) {
	if os.Getenv("REPLAY_DUMP") != "" {
		t.Logf("\n%s", out)
	}
}

func crashes(e *replayEnv) []string {
	var out []string
	for _, c := range device.LoadCrashes(e.st) {
		out = append(out, fmt.Sprintf("%s %s pid=%d %s %s %v", c.ID, c.Kind, c.PID, c.Process, c.Headline, c.Sources))
	}
	return out
}

func fxNow() time.Time { return time.Unix(1791572000, 0) }

func TestReplayExplore(t *testing.T) {
	t.Parallel()
	if os.Getenv("REPLAY_DUMP") == "" {
		t.Skip("set REPLAY_DUMP=1")
	}
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_native_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-SYSTEM_TOMBSTONE.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))
	for _, c := range crashes(e) {
		t.Log(c)
	}
	for _, c := range [][2]string{
		{"logcat_crashes", `{"package":"com.example.logtest"}`},
		{"logcat_app_launches", `{"package":"com.example.logtest","from":"2026-10-10 02:50:00"}`},
		{"logcat_status", `{"package":"com.example.logtest"}`},
		{"logcat_search", `{"filter":"tag=:LogTest tag=:MyRepo","from":"2026-10-10 03:07:00","limit":3}`},
		{"logcat_crashes", `{"kinds":["native"]}`},
	} {
		out, _ := e.call(c[0], c[1])
		t.Logf("=== %s %s\n%s", c[0], c[1], out)
	}
}

const launches = "logcat_app_launches"

func countOf(s, sub string) int { return strings.Count(s, sub) }

// §4.1: am_on_*_called (API 29) + am_activity_launch_time + Displayed; COLD.
func TestReplayLaunchCold(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("coldstart.logcat.txt")
	out := e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 02:59:00","to":"2026-10-10 03:00:14"}`,
		"1 launches", "COLD  com.example.logtest/.MainActivity  (pid 7182)",
		"+3722 ms   process started", "+13469 ms  MainActivity.onCreate  returned",
		"+13565 ms  onStart", "+13681 ms  onResume",
		"TTID (time to initial display, system-measured): 18692 ms   [source: ActivityTaskManager Displayed, logged at +20463 ms]",
		"am_on_*_called without callback durations", "how to read")
	notContains(t, out, "not found", "not logged", "Displayed (TTID)")
	if n := countOf(out, "18692"); n != 1 { // one clearly labelled number, not Displayed + launch_time side by side
		t.Errorf("18692 shown %d times:\n%s", n, out)
	}
	dump(t, out)
}

// §4.1: HOT = am_on_restart/start/resume, no Displayed line.
func TestReplayLaunchHot(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	e.file("hotstart.logcat.txt")
	out := e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 03:24:00"}`,
		"HOT  com.example.logtest/.MainActivity  (pid 3418)", "onRestart", "+855 ms",
		"+968 ms", "no Displayed line: expected for HOT starts")
	notContains(t, out, "UNKNOWN", "COLD", "WARM", "process started")
	dump(t, out)
}

// §4.1: Displayed logged >10 s after onResume; also must be found outside the request window.
func TestReplayLaunchLateDisplayed(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	out := e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 02:56:00","to":"2026-10-10 02:56:40"}`,
		"COLD  com.example.logtest/.MainActivity  (pid 6713)", "+18556 ms  MainActivity.onCreate",
		"+18942 ms  onResume", "TTID (time to initial display, system-measured): 26274 ms", "logged at +40420 ms")
	dump(t, out)
	// the capture that ended before Displayed was logged says so instead of guessing
	e2 := newReplay(t, fxBoot1)
	e2.file("coldstart1.logcat.txt")
	e2.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 02:56:00"}`,
		"COLD", "TTID: not logged", "Displayed not logged (yet)")
}

func TestReplayLaunchAfterReboot(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	e.file("coldstart_after_reboot.logcat.txt")
	e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 03:22:00"}`,
		"COLD  com.example.logtest/.MainActivity  (pid 3418)", "MainActivity.onCreate  returned",
		"TTID (time to initial display, system-measured): 22707 ms", "logged at +34938 ms")
}

// The "warm start" that turned into onNewIntent: must not be called COLD/WARM/HOT.
func TestReplayLaunchNewIntent(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	e.file("warmstart_FAILED_was_onNewIntent.logcat.txt")
	out := e.ok(launches, `{"package":"com.example.logtest","from":"2026-10-10 03:25:00"}`,
		"#1 2026-10-10 03:25:21.808  UNKNOWN  com.example.logtest/.MainActivity  (pid 3418) — not a launch", "onNewIntent", "LaunchState UNKNOWN")
	notContains(t, out, "COLD", "  WARM  ", "  HOT  ")
}

func pkgCrashes(e *replayEnv) []string {
	var r []string
	for _, c := range crashes(e) {
		if strings.Contains(c, fxPkg) {
			r = append(r, c)
		}
	}
	return r
}

// §4.2: am_crash [pid,user,process,flags,exc,msg,file,line] merges with FATAL EXCEPTION and dropbox.
func TestReplayJavaCrash(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	cs := pkgCrashes(e)
	if len(cs) != 1 || !strings.Contains(cs[0], "java pid=7182") ||
		!strings.Contains(cs[0], "events:am_crash") || !strings.Contains(cs[0], "dropbox:data_app_crash") {
		t.Fatalf("want one merged java crash for 7182, got %q", cs)
	}
	out := e.ok("logcat_crashes", `{"package":"com.example.logtest"}`,
		"1 match → full detail", "Java crash", "pid 7182 com.example.logtest",
		"sources: logcat:crash, events:am_crash, dropbox:data_app_crash", "Caused by: java.lang.IllegalStateException: repo not ready")
	notContains(t, out, "pid 0", "only am_crash")
}

// §4.2: native crash = one native entry (am_crash "Native crash" carries system_server's
// pid and is merged), DEBUG lines are crash_dump's (attributed to the app by uid),
// location = first app frame, SYSTEM_TOMBSTONE attached.
func TestReplayNativeCrash(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("native_crash.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_native_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-SYSTEM_TOMBSTONE.txt"))
	cs := pkgCrashes(e)
	if len(cs) != 1 || !strings.Contains(cs[0], "native pid=7736") {
		t.Fatalf("want exactly one native crash, got %q", cs)
	}
	for _, s := range []string{"events:am_crash", "dropbox:SYSTEM_TOMBSTONE", "logcat:tombstone"} {
		if !strings.Contains(cs[0], s) {
			t.Errorf("sources lack %s: %s", s, cs[0])
		}
	}
	det := e.ok("logcat_crashes", `{"package":"com.example.logtest"}`, "1 match → full detail",
		"native crash", "pid 7736 com.example.logtest", "SIGSEGV",
		"crash location: liblogtest.so [#00 pc 7fc]", "Cause: null pointer dereference", "SYSTEM_TOMBSTONE")
	notContains(t, det, "Native crash", "Java crash", "pid 4944")
	dump(t, det)
	s := e.ok("logcat_search", `{"package":"com.example.logtest","filter":"tag:DEBUG"}`, "crash_dump", "backtrace:")
	notContains(t, s, "7763/7763 com.example.logtest ")
}

// §4.2: SIGABRT has no "fault addr --------"; location is the first app frame (#02), not libc.
func TestReplayNativeAbort(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	e.file("native_abort.logcat.txt")
	cs := pkgCrashes(e)
	if len(cs) != 1 || !strings.Contains(cs[0], "native pid=3418") || !strings.Contains(cs[0], "SIGABRT") {
		t.Fatalf("want one SIGABRT crash for 3418, got %q", cs)
	}
	det := e.ok("logcat_crashes", `{"package":"com.example.logtest"}`, "1 match → full detail", "SIGABRT",
		"crash location: liblogtest.so", "#02 pc")
	notContains(t, det, "signal: SIGABRT (SI_QUEUE) fault addr", "--------  ", "crash location: libc", "Native crash")
	if !strings.Contains(det, "#02 pc 000000000000086f") {
		t.Errorf("tombstone excerpt lacks the backtrace:\n%s", det)
	}
	dump(t, det)
}

// §4.2: the :remote crashes group together and never with "repo not ready".
func TestReplayCrashGrouping(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	out := e.ok("logcat_crashes", `{"package":"com.example.logtest"}`,
		"4 crash groups (5 events)", "pid 8076 com.example.logtest:remote", "same signature also at 03:07:39 (cr4)")
	dump(t, out)
	// the "repo not ready" entry (cr2) must have no "same signature" line
	i := strings.Index(out, "cr2  ")
	if i < 0 {
		t.Fatalf("no cr2:\n%s", out)
	}
	if strings.Contains(out[i:], "same signature") {
		t.Errorf("repo-not-ready crash grouped with something:\n%s", out[i:])
	}
	for _, c := range crashes(e) {
		if strings.Contains(c, "Native crash") || strings.Contains(c, "pid=0 ") {
			t.Errorf("bogus entry %s", c)
		}
	}
}

func TestReplayANR(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("anr_broadcast.logcat.txt")
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))
	e.ok("logcat_crashes", `{"package":"com.example.logtest","kinds":["anr"]}`,
		"ANR · 2026-10-10 03:05:53.236 · pid 7815 com.example.logtest", "sources: events:am_anr, logcat:system, dropbox:data_app_anr",
		"cmp=com.example.logtest/.TriggerReceiver", "main thread [traces: data_app_anr]", `"main" prio=5 tid=1 Sleeping`)
}

// psRows builds `ps -A -o PID,UID,NAME` output: real system processes of the
// capture plus filler so the snapshot counts as complete (>= 20 rows).
func psRows(extra ...string) string {
	rows := []string{"  PID   UID NAME", "    1     0 init", " 1742  1000 zygote64", " 4944  1000 system_server",
		" 2225 10110 com.android.systemui", " 5752  2000 adbd"}
	for i := 0; i < 20; i++ {
		rows = append(rows, fmt.Sprintf("%5d  1000 android.hardware.svc%d", 300+i, i))
	}
	return strings.Join(append(rows, extra...), "\n") + "\n"
}

func sec(f float64) int64 { return int64(f * 1e9) }

// §4.3: ps seeds at awkward moments (<pre-initialized>, before am_proc_start,
// during a native crash with crash_dump64 and a phantom pid), force-stop,
// deaths, and processes still alive across a reboot.
func replayWithSnapshots(t *testing.T) *replayEnv {
	raw := fixtureFile(t, "session-raw.log")
	e := newReplay(t, fxBoot1)
	a, rest := splitAt(raw, 1791572193.5)
	e.replay(a)
	e.pipe.SeedPS(psRows(" 6713 10146 <pre-initialized>"), sec(1791572193.5))
	b, rest := splitAt(rest, 1791572355.2)
	e.replay(b)
	e.pipe.SeedPS(psRows(" 7182 10146 <pre-initialized>"), sec(1791572355.2))
	c, rest := splitAt(rest, 1791572565)
	e.replay(c)
	e.pipe.SeedPS(psRows(" 7736 10146 com.example.logtest", " 7763 10146 crash_dump64", " 7765 10146 com.example.logtest"), sec(1791572565))
	d, rest := splitAt(rest, 1791572940)
	e.replay(d)
	e.pipe.SeedPS(psRows(" 7815 10146 com.example.logtest", " 8076 10146 com.example.logtest:remote"), sec(1791572940))
	e.replay(rest)
	return e
}

func TestReplayProcesses(t *testing.T) {
	t.Parallel()
	e := replayWithSnapshots(t)
	out := e.ok("logcat_status", `{"package":"com.example.logtest"}`,
		"running when the device was last seen: pid 8076 com.example.logtest:remote (since 2026-10-10 03:07:42), pid 7815 com.example.logtest (since 2026-10-10 03:03:44)",
		"pid 6713 com.example.logtest (02:56:33–02:57:57, force-stopped)",
		"pid 7182 com.example.logtest (02:59:15–03:01:33, died; crashed",
		"pid 7815 com.example.logtest (since")
	dump(t, out)
	// 7765 = debuggerd's short-lived fork of the crashing 7736 (same name, no am_proc_start)
	// pid N (not a bare number: the store path names a random temp dir)
	notContains(t, out, "<pre-initialized>", "pid reused", "crash_dump64", "before recording", "pid 7765", "pid 7763")
	if regexp.MustCompile(`running[^\n]*(7765|6713|7763)`).MatchString(out) {
		t.Errorf("dead/helper pid listed as running:\n%s", out)
	}
}

func TestReplayProcessesReboot(t *testing.T) {
	t.Parallel()
	e := replayWithSnapshots(t)
	e.newSession(fxBoot2) // reboot (boot bf71fe22)
	e.file("session2-raw.log")
	out := e.ok("logcat_status", `{"package":"com.example.logtest"}`, "installed (uid 10146)",
		"has no running process", "before the last reboot: pid 8076 com.example.logtest:remote (03:07:42–03:10:33, device rebooted",
		"pid 7815 com.example.logtest (03:03:44–03:10:33, device rebooted; ANR: see logcat_crashes id=anr3)", "[boot 07a6d2d3]")
	dump(t, out)
	notContains(t, out, "<pre-initialized>", "pid reused", "crash_dump64", "running when")
}

// Same capture, status before the reboot: the live processes are listed as running.
func TestReplayProcessesNoReboot(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	e.pipe.SeedPS(psRows(" 7815 10146 com.example.logtest", " 8076 10146 com.example.logtest:remote"), sec(1791573033))
	out := e.ok("logcat_status", `{"package":"com.example.logtest"}`,
		"pid 8076 com.example.logtest:remote (since 2026-10-10 03:07:42)", "pid 7815 com.example.logtest (since 2026-10-10 03:03:44)",
		"pid 8029 com.example.logtest:remote (03:07:04–03:07:40, died; crashed")
	notContains(t, out, "pid reused", "<pre-initialized>")
}

// §4.6: -v printable octal escapes are decoded to UTF-8 for storage, matching and display.
func TestReplayPrintable(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	a, _ := splitAt(fixtureFile(t, "session3-raw.log"), 1791573720)
	e.replay(a)
	out := e.ok("logcat_search", `{"filter":"tag:KeyguardClockSwitch message:\"clock: 3\uee0121\"","from":"2026-10-10 03:21:00"}`,
		"Updating clock: 3\uee0121")
	notContains(t, out, `\356`, "matched 0")
}

// §4.3: `log -t` lines from the shell have no known process; label them by uid.
func TestReplayShellLines(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.replay("--------- beginning of main\n" +
		"         1791572937.282000 shell  8182  8182 I LOGMCP_MARK: hello mark from shell\n" +
		"         1791572937.504000 shell  8183  8183 E LOGMCP_MARK: error mark\n")
	e.ok("logcat_search", `{"filter":"tag:LOGMCP_MARK","from":"2026-10-10 03:08:00"}`,
		"8182/8182 uid:shell LOGMCP_MARK: hello mark", "8183/8183 uid:shell")
}

// §4.5: the echo shows explicit parentheses matching real precedence.
func TestReplayFilterEcho(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("remote.logcat.txt")
	e.ok("logcat_search", `{"filter":"tag=:LogTest tag=:MyRepo","from":"2026-10-10 03:07:00"}`,
		`filter parsed="(tag == \"LogTest\" OR tag == \"MyRepo\") AND device time in [`)
	e.ok("logcat_search", `{"filter":"tag=:LogTest | message:remote level:I","from":"2026-10-10 03:07:00"}`,
		`filter parsed="(tag == \"LogTest\" OR message contains \"remote\") AND level >= INFO AND device time in [`)
}

// Live regression (v2 e2e): the recorder's first ps snapshot is taken while the
// -D backlog is still being replayed; it must not pre-empt the am_proc_died /
// zygote exit lines that are still queued.
func TestReplayPSDuringBacklog(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot3)
	a, rest := splitAt(fixtureFile(t, "session3-raw.log"), 1791573900) // 3418 alive here
	e.replay(a)
	e.pipe.SeedPS(psRows(), sec(1791574767)) // snapshot taken "now" (03:39:27), 3418 already gone
	e.replay(rest)
	out := e.ok("logcat_status", `{"package":"com.example.logtest"}`,
		"pid 3418 com.example.logtest (03:22:31–03:26:20, exited (signal 6); crashed")
	notContains(t, out, "gone (not in ps)")
}

// Live regression (v2 e2e): DropBox history is fetched while the logcat
// backlog is still replaying, so DropBox can know a crash before logcat does.
// The logcat crash must adopt the DropBox entry instead of duplicating it.
func TestReplayDropboxBeforeLogcat(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_native_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-SYSTEM_TOMBSTONE.txt"))
	e.file("java_crash.logcat.txt", "native_crash.logcat.txt")
	cs := pkgCrashes(e)
	if len(cs) != 2 {
		t.Fatalf("want 2 crashes (java 7182, native 7736), got %d:\n%s", len(cs), strings.Join(cs, "\n"))
	}
	for _, c := range cs {
		if !strings.Contains(c, "logcat:crash") || !strings.Contains(c, "dropbox:") {
			t.Errorf("not merged: %s", c)
		}
	}
	e.ok("logcat_crashes", `{"package":"com.example.logtest"}`, "2 crash groups (2 events)")
}

// Live regression (v2 e2e): DropBox merged into a crash, then a later logcat
// update of the same crash (am_crash arrives ~10 s after the tombstone under
// TCG) must not drop the DropBox sources/tombstone.
func TestReplayDropboxThenLogcatUpdate(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	a, rest := splitAt(fixtureFile(t, "native_crash.logcat.txt"), 1791572565) // after DEBUG, before am_crash
	e.replay(a)
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_native_crash.txt"))
	e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-SYSTEM_TOMBSTONE.txt"))
	e.replay(rest)
	cs := pkgCrashes(e)
	if len(cs) != 1 {
		t.Fatalf("want 1 crash, got %q", cs)
	}
	for _, s := range []string{"events:am_crash", "dropbox:data_app_native_crash", "dropbox:SYSTEM_TOMBSTONE"} {
		if !strings.Contains(cs[0], s) {
			t.Errorf("lost %s: %s", s, cs[0])
		}
	}
}
