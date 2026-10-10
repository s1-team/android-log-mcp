package compat

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// api29 is a real probe of the API 29 emulator (android-log-mcp compat --raw).
func api29(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../testdata/compat/api29-emulator-5556.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n") // Windows checkouts
}

// with replaces section name's body and its "@@rc" line (rc < 0: none).
func with(raw, name, body string, rc int) string {
	re := regexp.MustCompile(`(?s)@@` + name + `\n.*?\n(@@[a-z]+\n)`)
	m := re.FindStringSubmatchIndex(raw)
	if m == nil {
		panic("no section " + name)
	}
	repl := "@@" + name + "\n" + body + "\n"
	if rc >= 0 {
		repl += "@@rc " + strconv.Itoa(rc) + "\n"
	}
	return raw[:m[0]] + repl + raw[m[2]:]
}

func status(r Result, name string) string {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return "absent"
}

const wmTags = `30008 am_anr (User|1|5),(pid|1|5),(Package Name|3),(Flags|1|5),(reason|3)
30011 am_proc_died (User|1|5),(PID|1|5),(Process Name|3),(OomAdj|1|5),(ProcState|1|5)
30014 am_proc_start (User|1|5),(PID|1|5),(UID|1|5),(Process Name|3),(Type|3),(Component|3)
30023 am_kill (User|1|5),(PID|1|5),(Process Name|3),(OomAdj|1|5),(Reason|3)
30039 am_crash (User|1|5),(PID|1|5),(Process Name|3),(Flags|1|5),(Exception|3),(Message|3),(File|3),(Line|1|5)
30021 wm_on_paused_called (Token|1|5),(Component Name|3),(Reason|3),(time|2|3)
30022 wm_on_resume_called (Token|1|5),(Component Name|3),(Reason|3),(time|2|3)
30057 wm_on_create_called (Token|1|5),(Component Name|3),(Reason|3),(time|2|3)
30059 wm_on_start_called (Token|1|5),(Component Name|3),(Reason|3),(time|2|3)
30009 wm_activity_launch_time (User|1|5),(Token|1|5),(Component Name|3),(time|2|3)`

const legacyPS = `USER      PID   PPID  VSZ    RSS   WCHAN            ADDR S NAME
root      1     0     27588  3164  SyS_epoll_wait   0 S init
system    612   305   1803460 98764 SyS_epoll_wait  0 S system_server
u0_a54    1234  305   1290448 61620 SyS_epoll_wait  0 S com.foo`

func TestParseAcrossAPILevels(t *testing.T) {
	base := api29(t)
	cases := []struct {
		name    string
		raw     string
		verdict string
		want    map[string]string
	}{
		{"API 29 emulator (real probe)", base, "SUPPORTED:", map[string]string{
			"logcat format": OK, "log buffers": OK, "logcat -T <epoch>": OK, "logcat --uid": Info,
			"lifecycle events": OK, "activity/process events": OK, "ps -A -o PID,UID,NAME": OK,
			"pm list packages -U": OK, "dumpsys dropbox": OK, "date +%s.%N / +%z": OK, "boot_id": OK, "am start -W": OK}},
		{"API 30+ layout (wm_on_*, --uid available) [synthetic]",
			with(with(with(base, "sdk", "35", -1), "tags", wmTags, -1), "uidfilter", "", 0), "SUPPORTED:",
			map[string]string{"lifecycle events": OK, "activity/process events": OK, "logcat --uid": Info}},
		{"pre-O style ps (no -A -o) [synthetic]",
			with(with(base, "ps", "", -1), "pslegacy", legacyPS, -1), "SUPPORTED (degraded)",
			map[string]string{"ps -A -o PID,UID,NAME": Fallback}},
		{"pm without -U, cmd package with uids [synthetic]",
			with(base, "pm", "", -1), "SUPPORTED (degraded)",
			map[string]string{"pm list packages -U": Fallback}},
		{"no uid in package list [synthetic]",
			with(with(base, "pm", "package:com.foo\npackage:com.bar", -1), "cmdpackage", "", -1), "SUPPORTED (degraded)",
			map[string]string{"pm list packages -U": Degraded}},
		{"dropbox permission denied [synthetic]",
			with(base, "dropbox", "Permission Denial: can't dump DropBoxManagerService from pid=1, uid=2000", -1), "SUPPORTED (degraded)",
			map[string]string{"dumpsys dropbox": Degraded}},
		{"logcat without the -v adverbs [synthetic]",
			with(base, "format", "Unrecognized Option uid", 1), "NOT SUPPORTED",
			map[string]string{"logcat format": Missing}},
		{"no crash buffer [synthetic]",
			with(base, "buffers", "main: ring buffer is 256 KiB (200 KiB consumed), max entry is 5120 B, max payload is 4076 B\nsystem: ring buffer is 256 KiB (10 KiB consumed), max entry is 5120 B, max payload is 4076 B\nevents: ring buffer is 256 KiB (100 KiB consumed), max entry is 5120 B, max payload is 4076 B", 0),
			"SUPPORTED (degraded)", map[string]string{"log buffers": Degraded}},
		{"-T <epoch> rejected [synthetic]", with(base, "resume", "", 1), "SUPPORTED (degraded)",
			map[string]string{"logcat -T <epoch>": Degraded}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Parse(c.raw)
			if !strings.HasPrefix(r.Summary, c.verdict) {
				t.Errorf("summary %q, want prefix %q\n%s", r.Summary, c.verdict, r.Text())
			}
			for k, v := range c.want {
				if got := status(r, k); got != v {
					t.Errorf("%s: %s, want %s", k, got, v)
				}
			}
		})
	}
	r := Parse(base)
	if r.SDK != 29 || r.Release != "10" || !strings.Contains(r.Fingerprint, ":10/") || r.ABI != "x86_64" {
		t.Errorf("device info: %+v", r)
	}
	for _, c := range r.Checks {
		if c.Name == "lifecycle events" && !strings.Contains(c.Detail, "am_on_") {
			t.Errorf("API 29 lifecycle: %s", c.Detail)
		}
	}
}
