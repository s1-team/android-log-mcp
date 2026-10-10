package analysis

import (
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// The launch analyser has no API-level switch: it accepts every event
// family/field layout seen across Android releases. One cold launch rendered
// in each layout must give the same timeline.
func TestLaunchEventFormatsAcrossAPILevels(t *testing.T) {
	type tc struct {
		name, atm, lifecycle, events string
		createMs                     int64 // -1 = no duration in this format
	}
	cases := []tc{
		{"API 29 (am_on_* [user,class,reason], ActivityTaskManager)", "ActivityTaskManager",
			"am_on_create_called: [0,com.foo.ui.MainActivity,performCreate]\n" +
				"%T.431000 10123 23011 23011 I am_on_start_called: [0,com.foo.ui.MainActivity,handleStartActivity]\n" +
				"%T.440000 10123 23011 23011 I am_on_resume_called: [0,com.foo.ui.MainActivity,RESUME_ACTIVITY]", "am_on_*", -1},
		{"API ≤ 28 style (am_on_*, ActivityManager tag)", "ActivityManager",
			"am_on_create_called: [0,com.foo.ui.MainActivity,performCreate]\n" +
				"%T.431000 10123 23011 23011 I am_on_start_called: [0,com.foo.ui.MainActivity,handleStartActivity]\n" +
				"%T.440000 10123 23011 23011 I am_on_resume_called: [0,com.foo.ui.MainActivity,RESUME_ACTIVITY]", "am_on_*", -1},
		{"API 30–35 (wm_on_* with durations)", "ActivityTaskManager",
			"wm_on_create_called: [123,com.foo.ui.MainActivity,performCreate,186]\n" +
				"%T.431000 10123 23011 23011 I wm_on_start_called: [123,com.foo.ui.MainActivity,handleStartActivity,9]\n" +
				"%T.440000 10123 23011 23011 I wm_on_resume_called: [123,com.foo.ui.MainActivity,RESUME_ACTIVITY,6]", "wm_on_*", 186},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := "--------- beginning of system\n" +
				"1791570500.000000  1000  1880  2100 I " + c.atm + ": START u0 {act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] flg=0x10200000 cmp=com.foo/.ui.MainActivity} from uid 2000\n" +
				"--------- switch to events\n" +
				"1791570500.038000  1000  1880  1900 I am_proc_start: [0,23011,10123,com.foo,activity,{com.foo/com.foo.ui.MainActivity}]\n" +
				"1791570500.412000 10123 23011 23011 I " + strings.ReplaceAll(c.lifecycle, "%T", "1791570500") + "\n" +
				"--------- switch to system\n" +
				"1791570500.812000  1000  1880  1920 I " + c.atm + ": Displayed com.foo/.ui.MainActivity: +812ms"
			ls := Launches(records(t, log), "com.foo")
			if len(ls) != 1 {
				t.Fatalf("%d launches", len(ls))
			}
			l := ls[0]
			if l.Kind != "COLD" || l.PID != 23011 || l.Displayed.TookMs != 812 || l.Events != c.events {
				t.Fatalf("kind %s pid %d displayed %d events %q", l.Kind, l.PID, l.Displayed.TookMs, l.Events)
			}
			if l.OnCreate.Ns == 0 || l.OnResume.Ns == 0 || (l.OnResume.Ns-l.RequestNs)/1e6 != 440 {
				t.Fatalf("lifecycle steps missing: %+v", l)
			}
			if l.OnCreate.TookMs != c.createMs {
				t.Errorf("onCreate took %d, want %d", l.OnCreate.TookMs, c.createMs)
			}
			if c.events == "am_on_*" && !strings.Contains(strings.Join(l.Notes, " "), "without callback durations") {
				t.Errorf("missing am_on_* note: %v", l.Notes)
			}
		})
	}
}

// am_crash: [PID,User,…] observed on API 29, [User,PID,…] per AOSP's
// EventLogTags (and other releases); the pid is chosen by value.
func TestAmCrashFieldOrderAcrossAPILevels(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		pid       int
	}{
		{"API 29 observed [pid,user]", "[4321,0,com.foo,952745540,java.lang.IllegalStateException,boom,Main.kt,10]", 4321},
		{"EventLogTags [user,pid]", "[0,4321,com.foo,952745540,java.lang.IllegalStateException,boom,Main.kt,10,0]", 4321},
		{"secondary user [user=10,pid]", "[10,4321,com.foo,952745540,java.lang.IllegalStateException,boom,Main.kt,10,0]", 4321},
		{"secondary user [pid,user=10]", "[4321,10,com.foo,952745540,java.lang.IllegalStateException,boom,Main.kt,10]", 4321},
	} {
		pid, _, proc, ok := procs.AmCrashPID(c.msg)
		if !ok || pid != c.pid || proc != "com.foo" {
			t.Errorf("%s: pid %d proc %q ok %v", c.name, pid, proc, ok)
		}
	}
}
