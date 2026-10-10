package analysis

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Step is one timeline point of a launch.
type Step struct {
	Name   string
	Ns     int64 // 0 = not found
	TookMs int64 // callback duration if reported (-1 unknown)
	Note   string
}

// Launch is one computed app launch.
type Launch struct {
	Package    string
	Component  string // com.foo/.ui.MainActivity as logged
	Class      string // com.foo.ui.MainActivity
	RequestNs  int64
	RequestMsg string
	Kind       string // COLD WARM HOT UNKNOWN
	KindWhy    string // evidence for Kind
	PID        int
	ProcStart  Step
	OnCreate   Step
	OnRestart  Step
	OnStart    Step
	OnResume   Step
	Displayed  Step // TookMs = system-measured TTID (Displayed line or *_activity_launch_time)
	FullyDrawn Step
	Events     string // "am_on_*" (API ≤ 29, no durations) or "wm_on_*"
	Notes      []string
}

var (
	reStart     = regexp.MustCompile(`^START u\d+ \{.*?\bcmp=([^\s}]+)`)
	reDisplayed = regexp.MustCompile(`^(Displayed|Fully drawn) (\S+?)(?: for user \d+)?: \+(\S+?)(?:\s|$)`)
	reDurPart   = regexp.MustCompile(`(\d+)(ms|s|m)`)
)

// ParseAndroidDuration parses "+1s20ms" style durations (without '+') to ms.
func ParseAndroidDuration(s string) int64 {
	s = strings.TrimPrefix(s, "+")
	var total int64
	for _, m := range reDurPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		switch m[2] {
		case "ms":
			total += n
		case "s":
			total += n * 1000
		case "m":
			total += n * 60000
		}
	}
	return total
}

// ExpandComponent turns "com.foo/.Main" into ("com.foo", "com.foo.Main").
func ExpandComponent(cmp string) (pkg, cls string) {
	i := strings.IndexByte(cmp, '/')
	if i < 0 {
		return "", cmp
	}
	pkg, cls = cmp[:i], cmp[i+1:]
	if strings.HasPrefix(cls, ".") {
		cls = pkg + cls
	}
	return pkg, cls
}

func isATM(tag string) bool { return tag == "ActivityTaskManager" || tag == "ActivityManager" }

// LaunchTag reports whether a record can contribute to a launch timeline.
func LaunchTag(r *model.Record) bool {
	switch r.Tag {
	case "ActivityTaskManager", "ActivityManager":
		return strings.HasPrefix(r.Msg, "START u") || strings.HasPrefix(r.Msg, "Displayed ") ||
			strings.HasPrefix(r.Msg, "Fully drawn ") || strings.HasPrefix(r.Msg, "Start proc ")
	case "am_proc_start", "am_activity_launch_time", "wm_activity_launch_time":
		return true
	}
	return lifecycleStep(r.Tag) != ""
}

// lifecycleStep maps both event families to a step name:
// wm_on_<x>_called (Android 11+, with a duration field) and
// am_on_<x>_called (Android ≤ 10, "[0,class,reason]", no duration).
func lifecycleStep(tag string) string {
	var rest string
	switch {
	case strings.HasPrefix(tag, "wm_on_"):
		rest = tag[len("wm_on_"):]
	case strings.HasPrefix(tag, "am_on_"):
		rest = tag[len("am_on_"):]
	default:
		return ""
	}
	switch rest {
	case "create_called", "restart_called", "start_called", "resume_called", "paused_called":
		return strings.TrimSuffix(rest, "_called")
	}
	return ""
}

// DisplayedSlackNs: Displayed is logged when the first frame is drawn and,
// under load, often many seconds after onResume; search this long after the
// request (bounded by the next launch request of the same component).
const DisplayedSlackNs = int64(180e9)

// Launches computes launches from records (any order; they are sorted by
// time here because logcat interleaves buffers non-monotonically).
// pkg=="" means all.
func Launches(in []*model.Record, pkg string) []*Launch {
	recs := make([]*model.Record, 0, len(in))
	for _, r := range in {
		if LaunchTag(r) {
			recs = append(recs, r)
		}
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TimeNs < recs[j].TimeNs })
	type anchor struct {
		idx int
		l   *Launch
	}
	var anchors []anchor
	for i, r := range recs {
		if !isATM(r.Tag) || !strings.HasPrefix(r.Msg, "START u") {
			continue
		}
		m := reStart.FindStringSubmatch(r.Msg)
		if m == nil {
			continue
		}
		p, cls := ExpandComponent(m[1])
		if pkg != "" && p != pkg {
			continue
		}
		anchors = append(anchors, anchor{i, &Launch{Package: p, Component: m[1], Class: cls, RequestNs: r.TimeNs, RequestMsg: r.Msg, Kind: "UNKNOWN"}})
	}
	var out []*Launch
	for ai, a := range anchors {
		l := a.l
		// lifecycle evidence belongs to this request until the next request
		// for the same package; Displayed may come much later (see above).
		end := l.RequestNs + 60e9
		dispEnd := l.RequestNs + DisplayedSlackNs
		for _, b := range anchors[ai+1:] {
			if b.l.Package == l.Package {
				end = min(end, b.l.RequestNs)
				if b.l.Class == l.Class {
					dispEnd = min(dispEnd, b.l.RequestNs)
				}
				break
			}
		}
		for _, s := range []*Step{&l.ProcStart, &l.OnCreate, &l.OnRestart, &l.OnStart, &l.OnResume, &l.Displayed, &l.FullyDrawn} {
			s.TookMs = -1
		}
		appPIDs := map[int]bool{}
		paused := false // the target activity itself paused first (intent delivered to it)
		var launchTimeMs int64 = -1
		var launchTimeNs int64
		for _, r := range recs[a.idx+1:] {
			if r.TimeNs >= max(end, dispEnd) {
				break
			}
			inMain := r.TimeNs < end
			switch {
			case r.Tag == "am_proc_start" && l.ProcStart.Ns == 0 && inMain:
				f := logcat.EventFields(r.Msg, 6)
				if len(f) >= 4 && (f[3] == l.Package || strings.HasPrefix(f[3], l.Package+":")) &&
					(len(f) < 6 || strings.Contains(f[5], l.Package+"/")) {
					pid, _ := strconv.Atoi(f[1])
					l.PID = pid
					appPIDs[pid] = true
					l.ProcStart = Step{Name: "process started", Ns: r.TimeNs, TookMs: -1, Note: "am_proc_start pid " + f[1]}
				}
			case r.Tag == "ActivityManager" && strings.HasPrefix(r.Msg, "Start proc ") && l.ProcStart.Ns == 0 && inMain:
				if m := reStartProcLine.FindStringSubmatch(r.Msg); m != nil && (m[2] == l.Package || strings.HasPrefix(m[2], l.Package+":")) {
					pid, _ := strconv.Atoi(m[1])
					l.PID = pid
					appPIDs[pid] = true
					l.ProcStart = Step{Name: "process started", Ns: r.TimeNs, TookMs: -1, Note: "Start proc pid " + m[1]}
				}
			case lifecycleStep(r.Tag) != "" && inMain:
				f := logcat.EventFields(r.Msg, 0)
				if len(f) < 2 || !classMatches(f[1], l.Class) {
					continue
				}
				if !(r.Package == l.Package || appPIDs[r.PID] || strings.HasPrefix(r.Process, l.Package) || r.Process == "" && r.Package == "") {
					continue
				}
				took := int64(-1)
				if len(f) >= 4 {
					if n, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil {
						took = n
					}
				}
				var st *Step
				switch lifecycleStep(r.Tag) {
				case "paused":
					if l.OnResume.Ns == 0 {
						paused = true
					}
					continue
				case "create":
					st = &l.OnCreate
				case "restart":
					st = &l.OnRestart
				case "start":
					st = &l.OnStart
				case "resume":
					st = &l.OnResume
				}
				// only the first lifecycle pass after the request
				if st.Ns == 0 && l.OnResume.Ns == 0 {
					*st = Step{Ns: r.TimeNs, TookMs: took}
					l.Events = r.Tag[:2] + "_on_*"
					if l.PID == 0 {
						l.PID = r.PID
					}
				}
			case r.Tag == "am_activity_launch_time" || r.Tag == "wm_activity_launch_time":
				// [User/Token, hash, component, ms(, …)]
				f := logcat.EventFields(r.Msg, 0)
				if len(f) >= 4 && launchTimeNs == 0 {
					_, cls := ExpandComponent(f[2])
					if cls == l.Class {
						if n, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil {
							launchTimeMs, launchTimeNs = n, r.TimeNs
						}
					}
				}
			case isATM(r.Tag) && (strings.HasPrefix(r.Msg, "Displayed ") || strings.HasPrefix(r.Msg, "Fully drawn ")):
				m := reDisplayed.FindStringSubmatch(r.Msg)
				if m == nil {
					continue
				}
				_, cls := ExpandComponent(m[2])
				if cls != l.Class {
					continue
				}
				st := &l.Displayed
				if m[1] == "Fully drawn" {
					st = &l.FullyDrawn
				}
				if st.Ns == 0 {
					*st = Step{Ns: r.TimeNs, TookMs: ParseAndroidDuration(m[3]), Note: "ActivityTaskManager Displayed"}
					if m[1] == "Fully drawn" {
						st.Note = "Fully drawn"
					}
				}
			}
		}
		if l.Displayed.Ns == 0 && launchTimeNs != 0 {
			l.Displayed = Step{Ns: launchTimeNs, TookMs: launchTimeMs, Note: "activity_launch_time event"}
		}
		switch {
		case l.ProcStart.Ns != 0:
			l.Kind, l.KindWhy = "COLD", "a new process was started for it"
		case l.OnCreate.Ns != 0:
			l.Kind, l.KindWhy = "WARM", "process existed; activity was (re)created (onCreate)"
		case l.OnRestart.Ns != 0 || l.OnStart.Ns != 0:
			l.Kind, l.KindWhy = "HOT", "existing activity brought to front (onRestart/onStart, no onCreate)"
		case l.OnResume.Ns != 0 && paused:
			l.Kind, l.KindWhy = "UNKNOWN", "not a launch: the activity was already on top; the intent was delivered to it (onPause → onNewIntent → onResume). Android reports LaunchState UNKNOWN"
		case l.OnResume.Ns != 0:
			l.Kind, l.KindWhy = "HOT", "existing activity resumed (no onCreate)"
		}
		switch {
		case l.OnResume.Ns == 0 && l.Displayed.Ns == 0:
			l.Notes = append(l.Notes, "no lifecycle or Displayed evidence for this component after the request (launch redirected to an existing task — e.g. onNewIntent —, blocked, or the events buffer was not recorded)")
		case l.Displayed.Ns == 0 && l.Kind == "UNKNOWN" && paused:
			l.Notes = append(l.Notes, "no Displayed line: nothing new was drawn (am start -W reports LaunchState UNKNOWN / TotalTime 0); to measure a WARM start, finish the activity first (back) or use HOT via the home screen")
		case l.Displayed.Ns == 0 && l.Kind == "HOT":
			l.Notes = append(l.Notes, "no Displayed line: expected for HOT starts (the window was already drawn); request→onResume is the best available measure")
		case l.Displayed.Ns == 0:
			l.Notes = append(l.Notes, "Displayed not logged (yet): it can appear many seconds after onResume on a loaded device; query again later")
		}
		if l.Events == "am_on_*" {
			l.Notes = append(l.Notes, "this Android version logs am_on_*_called without callback durations")
		}
		out = append(out, l)
	}
	return out
}

var reStartProcLine = regexp.MustCompile(`^Start proc (\d+):([^/\s]+)/`)

func classMatches(logged, cls string) bool {
	if logged == cls {
		return true
	}
	// some versions log the short form or a "pkg/cls" component
	if strings.Contains(logged, "/") {
		_, c := ExpandComponent(logged)
		return c == cls
	}
	return strings.HasSuffix(cls, "."+logged)
}
