// Package compat probes a device for the adb shell features android-log-mcp
// depends on and evaluates them with the same parsers the recorder uses
// (logcat.ParseLine, procs.ParsePS, procs.ParsePackagesU, the launch
// analyser's lifecycle event families). It is read-only: it never clears
// logs, starts apps or changes settings.
package compat

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// Script is the single read-only `adb shell` command whose output Parse
// splits into sections ("@@name" lines; "@@rc N" records an exit code).
// It only uses commands the recorder itself runs, plus help/listing calls.
const Script = `echo @@sdk; getprop ro.build.version.sdk
echo @@release; getprop ro.build.version.release
echo @@fingerprint; getprop ro.build.fingerprint
echo @@abi; getprop ro.product.cpu.abi
echo @@format; logcat -d -t 20 -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable 2>&1; echo "@@rc $?"
echo @@resume; logcat -d -b main -v epoch -T "$(( $(date +%s) - 1 )).000000" >/dev/null 2>&1; echo "@@rc $?"
echo @@uidfilter; logcat -d -t 1 --uid=0 >/dev/null 2>&1; echo "@@rc $?"
echo @@buffers; logcat -g -b main,system,crash,events 2>&1; echo "@@rc $?"
echo @@ps; ps -A -o PID,UID,NAME 2>/dev/null
echo @@pslegacy; ps 2>/dev/null | head -n 400
echo @@pm; pm list packages -U 2>/dev/null | head -n 40
echo @@cmdpackage; cmd package list packages -U 2>/dev/null | head -n 40
echo @@dropbox; dumpsys dropbox 2>&1 | head -n 5
echo @@date; date +%s.%N; date +%z
echo @@bootid; cat /proc/sys/kernel/random/boot_id
echo @@tags; grep -E '^[0-9]+ (am|wm)_(on_[a-z_]+_called|proc_start|proc_died|kill|crash|anr|activity_launch_time|activity_fully_drawn_time) ' /system/etc/event-log-tags 2>&1
echo @@am; am 2>&1 | grep -E -- '^ *(start|-W)' | head -n 3
echo @@end`

// Status of one check.
const (
	OK       = "ok"       // works as the recorder expects
	Fallback = "fallback" // primary path missing; the documented fallback is used
	Degraded = "degraded" // works with reduced information
	Missing  = "missing"  // a feature the tool needs is unavailable
	Info     = "info"     // not used by the tool; reported for reference
)

// Check is one probed feature.
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	UsedFor string `json:"used_for"`
}

// Result is the evaluated probe of one device.
type Result struct {
	Serial      string  `json:"serial,omitempty"`
	ADB         string  `json:"adb,omitempty"`
	Tracking    string  `json:"track_devices,omitempty"`
	SDK         int     `json:"sdk"`
	Release     string  `json:"release"`
	Fingerprint string  `json:"fingerprint"`
	ABI         string  `json:"abi"`
	Checks      []Check `json:"checks"`
	Summary     string  `json:"summary"`
}

type section struct {
	out string
	rc  int // -1 when not recorded
}

func split(out string) map[string]section {
	m := map[string]section{}
	name := ""
	var b strings.Builder
	rc := -1
	flush := func() {
		if name != "" {
			m[name] = section{out: strings.TrimRight(b.String(), "\n"), rc: rc}
		}
		b.Reset()
		rc = -1
	}
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(ln, "@@rc ") {
			rc, _ = strconv.Atoi(strings.TrimSpace(ln[5:]))
			continue
		}
		if strings.HasPrefix(ln, "@@") && !strings.Contains(ln, " ") {
			flush()
			name = ln[2:]
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	flush()
	return m
}

var reBuffer = regexp.MustCompile(`(?m)^(\w+): ring buffer is ([0-9.]+ ?[KMG]?i?B)`)

// Parse evaluates the output of Script.
func Parse(out string) Result {
	s := split(out)
	r := Result{Release: s["release"].out, Fingerprint: s["fingerprint"].out, ABI: s["abi"].out}
	r.SDK, _ = strconv.Atoi(strings.TrimSpace(s["sdk"].out))
	add := func(name, status, used, f string, a ...any) {
		r.Checks = append(r.Checks, Check{Name: name, Status: status, UsedFor: used, Detail: fmt.Sprintf(f, a...)})
	}

	// logcat output format: the recorder's exact flags, parsed by its parser
	{
		sec := s["format"]
		parsed, withUID, total := 0, 0, 0
		unknown := map[string]bool{}
		for _, ln := range strings.Split(sec.out, "\n") {
			if ln == "" || strings.HasPrefix(ln, "--------- ") {
				continue
			}
			total++
			if p := logcat.ParseLine(ln, time.UTC, time.Now().Year()); p.OK {
				parsed++
				if p.UID >= 0 {
					withUID++
				} else if f := strings.Fields(ln); len(f) > 1 {
					unknown[f[1]] = true
				}
			}
		}
		used := "recording (logcat -v threadtime -v epoch -v uid -v usec -v printable)"
		switch {
		case sec.rc != 0:
			add("logcat format", Missing, used, "logcat exited %d: %s", sec.rc, firstLine(sec.out))
		case parsed == 0:
			add("logcat format", Missing, used, "%d lines, none parsed: %s", total, firstLine(sec.out))
		case withUID*10 < parsed*9:
			add("logcat format", Degraded, used, "%d/%d lines parsed, numeric uid on only %d (uid-based package mapping limited)", parsed, total, withUID)
		default:
			d := fmt.Sprintf("%d/%d lines parsed, uid column present", parsed, total)
			if len(unknown) > 0 {
				d += fmt.Sprintf(" (unmapped uid names: %s)", strings.Join(sortedKeys(unknown), ","))
			}
			add("logcat format", OK, used, "%s", d)
		}
	}
	// buffers
	{
		sec := s["buffers"]
		got := map[string]string{}
		for _, m := range reBuffer.FindAllStringSubmatch(sec.out, -1) {
			got[m[1]] = m[2]
		}
		var have, miss []string
		for _, b := range []string{"main", "system", "crash", "events"} {
			if v, ok := got[b]; ok {
				have = append(have, b+"="+v)
			} else {
				miss = append(miss, b)
			}
		}
		used := "recording -b main,system,crash,events"
		switch {
		case len(have) == 0:
			add("log buffers", Missing, used, "logcat -g: %s", firstLine(sec.out))
		case len(miss) > 0:
			add("log buffers", Degraded, used, "%s; missing %s", strings.Join(have, " "), strings.Join(miss, ","))
		default:
			add("log buffers", OK, used, "%s", strings.Join(have, " "))
		}
	}
	// resume with -T <epoch>
	if s["resume"].rc == 0 {
		add("logcat -T <epoch>", OK, "resuming after a restart without duplicates", "accepted")
	} else {
		add("logcat -T <epoch>", Degraded, "resuming after a restart without duplicates", "exit %d: resume falls back to a full dump; duplicates are dropped by time/seq", s["resume"].rc)
	}
	// --uid filter (not used)
	if s["uidfilter"].rc == 0 {
		add("logcat --uid", Info, "not used (filtering is host-side)", "available")
	} else {
		add("logcat --uid", Info, "not used (filtering is host-side)", "unavailable on this build (API 29 has no --uid)")
	}
	// lifecycle events
	tags := map[string]string{}
	for _, ln := range strings.Split(s["tags"].out, "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 {
			tags[f[1]] = strings.Join(f[2:], " ")
		}
	}
	{
		used := "launch timeline (onCreate/onStart/onResume)"
		_, wm := tags["wm_on_resume_called"]
		_, am := tags["am_on_resume_called"]
		switch {
		case len(tags) == 0:
			add("lifecycle events", Info, used, "event-log-tags not readable (%s); the analyser accepts both am_on_* and wm_on_*", firstLine(s["tags"].out))
		case wm:
			add("lifecycle events", OK, used, "wm_on_*_called (with callback durations)")
		case am:
			add("lifecycle events", OK, used, "am_on_*_called (API ≤ 29 format, no callback durations)")
		default:
			add("lifecycle events", Degraded, used, "no am_on_/wm_on_*_called tags; timelines use START/am_proc_start/Displayed only")
		}
		var lt []string
		for _, t := range []string{"wm_activity_launch_time", "am_activity_launch_time", "am_proc_start", "am_proc_died", "am_kill", "am_crash", "am_anr"} {
			if _, ok := tags[t]; ok {
				lt = append(lt, t)
			}
		}
		if len(tags) > 0 {
			st := OK
			if _, ok := tags["am_proc_start"]; !ok {
				st = Degraded
			}
			add("activity/process events", st, "COLD/WARM/HOT, process table, crashes, ANRs", "%s", strings.Join(lt, " "))
		}
		if v, ok := tags["am_crash"]; ok {
			add("am_crash fields", Info, "crash grouping (pid chosen by value, both orders handled)", "declared %s", firstField(v, 2))
		}
	}
	// process table
	{
		used := "pid → process name (30 s snapshots)"
		if e := procs.ParsePS(s["ps"].out); len(e) > 0 && e[0].UID >= 0 {
			add("ps -A -o PID,UID,NAME", OK, used, "%d processes parsed", len(e))
		} else if e := procs.ParsePS(s["pslegacy"].out); len(e) > 0 {
			add("ps -A -o PID,UID,NAME", Fallback, used, "legacy `ps` parsed (%d processes)", len(e))
		} else {
			add("ps -A -o PID,UID,NAME", Missing, used, "no usable ps output: %s", firstLine(s["ps"].out))
		}
	}
	// packages
	{
		used := "uid → package (crash/launch attribution, package: filter)"
		count := func(m map[int][]string) (withUID, without int) {
			for k, v := range m {
				if k < 0 {
					without += len(v)
				} else {
					withUID += len(v)
				}
			}
			return
		}
		if n, _ := count(procs.ParsePackagesU(s["pm"].out)); n > 0 {
			add("pm list packages -U", OK, used, "%d packages with uid (first 40 lines)", n)
		} else if n, _ := count(procs.ParsePackagesU(s["cmdpackage"].out)); n > 0 {
			add("pm list packages -U", Fallback, used, "cmd package list packages -U: %d packages with uid", n)
		} else if _, m := count(procs.ParsePackagesU(s["pm"].out)); m > 0 {
			add("pm list packages -U", Degraded, used, "packages listed without uid (%d); attribution by process name only", m)
		} else {
			add("pm list packages -U", Missing, used, "no package list: %s", firstLine(s["pm"].out))
		}
	}
	// dropbox
	{
		out := s["dropbox"].out
		used := "ANR traces, tombstones, crashes from before recording"
		switch {
		case strings.Contains(out, "Drop box contents"):
			add("dumpsys dropbox", OK, used, "%s", firstLine(out))
		case strings.Contains(out, "Permission Denial"):
			add("dumpsys dropbox", Degraded, used, "permission denied; crashes come from logcat only")
		default:
			add("dumpsys dropbox", Degraded, used, "unexpected output: %s", firstLine(out))
		}
	}
	// clock / boot id
	{
		l := strings.Split(s["date"].out, "\n")
		used := "device clock offset, time zone"
		if len(l) >= 2 && regexp.MustCompile(`^\d{9,}\.\d+$`).MatchString(strings.TrimSpace(l[0])) && regexp.MustCompile(`^[+-]\d{4}$`).MatchString(strings.TrimSpace(l[1])) {
			add("date +%s.%N / +%z", OK, used, "%s %s", strings.TrimSpace(l[0]), strings.TrimSpace(l[1]))
		} else {
			add("date +%s.%N / +%z", Degraded, used, "unexpected: %q", s["date"].out)
		}
		if b := strings.TrimSpace(s["bootid"].out); len(b) >= 32 {
			add("boot_id", OK, "reboot detection / sessions", "%s", b)
		} else {
			add("boot_id", Degraded, "reboot detection / sessions", "unreadable: %q", b)
		}
	}
	// am start -W (report --am-start only; never executed by the probe)
	if strings.Contains(s["am"].out, "-W") {
		add("am start -W", OK, "report --am-start only (not executed by the probe)", "listed in `am` help")
	} else {
		add("am start -W", Info, "report --am-start only (not executed by the probe)", "not found in `am` help output")
	}
	r.Summary = summarize(r)
	return r
}

func summarize(r Result) string {
	n := map[string][]string{}
	for _, c := range r.Checks {
		n[c.Status] = append(n[c.Status], c.Name)
	}
	verdict := "SUPPORTED"
	if len(n[Missing]) > 0 {
		verdict = "NOT SUPPORTED"
	} else if len(n[Degraded]) > 0 || len(n[Fallback]) > 0 {
		verdict = "SUPPORTED (degraded)"
	}
	var parts []string
	for _, st := range []string{OK, Fallback, Degraded, Missing, Info} {
		if len(n[st]) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", len(n[st]), st))
		}
	}
	s := fmt.Sprintf("%s: API %d (Android %s): %s", verdict, r.SDK, r.Release, strings.Join(parts, ", "))
	for _, st := range []string{Missing, Degraded, Fallback} {
		if len(n[st]) > 0 {
			sort.Strings(n[st])
			s += fmt.Sprintf("; %s: %s", st, strings.Join(n[st], ", "))
		}
	}
	return s
}

// Text renders r for humans.
func (r Result) Text() string {
	var b strings.Builder
	if r.Serial != "" {
		fmt.Fprintf(&b, "device       %s\n", r.Serial)
	}
	fmt.Fprintf(&b, "android      %s (API %d), abi %s\n", r.Release, r.SDK, r.ABI)
	fmt.Fprintf(&b, "fingerprint  %s\n", r.Fingerprint)
	if r.ADB != "" {
		fmt.Fprintf(&b, "adb          %s\n", r.ADB)
	}
	if r.Tracking != "" {
		fmt.Fprintf(&b, "track-devices %s\n", r.Tracking)
	}
	b.WriteString("\n")
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-9s %-26s %s\n          %-26s used for: %s\n", "["+c.Status+"]", c.Name, c.Detail, "", c.UsedFor)
	}
	fmt.Fprintf(&b, "\n%s\n", r.Summary)
	return b.String()
}

func sortedKeys(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

func firstField(v string, n int) string {
	f := strings.Split(v, "),")
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, "),") + "),…"
}
