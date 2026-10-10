// Package procs tracks pid -> process name / application id per boot, from
// events (am_proc_start, am_proc_died, am_kill), ActivityManager text lines,
// a `ps` baseline and the `pm list packages -U` uid map.
package procs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Proc is one process lifetime, keyed by (boot, pid, start).
type Proc struct {
	// ID is stable for the lifetime entry (name/start may be refined later,
	// e.g. a ps-seeded "<pre-initialized>" entry merged with am_proc_start).
	ID        string `json:"id,omitempty"`
	Boot      string `json:"boot"`
	PID       int    `json:"pid"`
	UID       int    `json:"uid"`
	Name      string `json:"name"`
	Package   string `json:"package,omitempty"`
	StartNs   int64  `json:"start_ns,omitempty"` // 0 = started before we looked (ps seed)
	EndNs     int64  `json:"end_ns,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
	Crashed   bool   `json:"crashed,omitempty"`
	Source    string `json:"source"`            // am_proc_start | Start proc | ps
	SeenNs    int64  `json:"seen_ns,omitempty"` // ps snapshot time for ps-seeded entries
}

// PreInitialized is the transient name of a freshly forked app process.
const PreInitialized = "<pre-initialized>"

func placeholderName(n string) bool { return n == "" || n == PreInitialized }

// Alive reports whether no end was observed.
func (p *Proc) Alive() bool { return p.EndNs == 0 }

// startSlackNs: a process may log before system_server logs its start.
const startSlackNs = int64(2e9)

// Tracker is safe for concurrent use.
type Tracker struct {
	mu       sync.RWMutex
	boot     string
	byPID    map[int][]*Proc // sorted by StartNs
	uidPkgs  map[int][]string
	packages map[string]bool
	// OnChange is called (without the lock) for every start/end so the caller can persist it.
	OnChange func(p Proc)
	nextID   int
	lastNs   int64 // newest record time observed (to detect a backlog)
}

// GoneNotInPS is the end reason for live entries a full ps snapshot no longer lists.
const GoneNotInPS = "gone (not in ps)"

// psCatchUpNs: a snapshot proves absence only if the log stream has caught up
// to within this much of the snapshot time; otherwise lifecycle lines still
// queued in the backlog (am_proc_died, …) would be pre-empted.
const psCatchUpNs = 30e9

func New() *Tracker {
	return &Tracker{byPID: map[int][]*Proc{}, uidPkgs: map[int][]string{}, packages: map[string]bool{}}
}

// Reset starts a new boot generation (pid table replaced).
func (t *Tracker) Reset(boot string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if boot == t.boot {
		return
	}
	t.boot = boot
	t.byPID = map[int][]*Proc{}
}

// Restore loads persisted lifetimes (e.g. procs.jsonl) for the current boot.
func (t *Tracker) Restore(ps []Proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range ps {
		p := ps[i]
		if p.Boot != t.boot {
			continue
		}
		t.upsertLocked(&p)
	}
}

func (t *Tracker) upsertLocked(p *Proc) {
	l := t.byPID[p.PID]
	for _, q := range l {
		if (p.ID != "" && q.ID == p.ID) || (p.ID == "" && q.StartNs == p.StartNs && q.Name == p.Name) {
			*q = *p
			t.sortLocked(p.PID)
			return
		}
	}
	if p.ID == "" {
		t.nextID++
		p.ID = fmt.Sprintf("%s/%d/%d.%d", p.Boot, p.PID, max(p.StartNs, p.SeenNs), t.nextID)
	}
	t.byPID[p.PID] = append(l, p)
	t.sortLocked(p.PID)
}

// order key: real start time, or the ps snapshot time for seeded entries
// (they started at some unknown earlier point).
func order(p *Proc) int64 {
	if p.StartNs != 0 {
		return p.StartNs
	}
	return p.SeenNs - 1
}

func (t *Tracker) sortLocked(pid int) {
	l := t.byPID[pid]
	sort.SliceStable(l, func(i, j int) bool { return order(l[i]) < order(l[j]) })
}

// SetPackages installs the uid map from `pm list packages -U`.
func (t *Tracker) SetPackages(uidPkgs map[int][]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.uidPkgs = uidPkgs
	t.packages = map[string]bool{}
	for _, l := range uidPkgs {
		for _, p := range l {
			t.packages[p] = true
		}
	}
	for _, l := range t.byPID {
		for _, p := range l {
			if p.Package == "" {
				p.Package = t.packageForLocked(p.Name, p.UID)
			}
		}
	}
}

// Installed reports whether pkg is in the last package list, and its uid.
func (t *Tracker) Installed(pkg string) (bool, int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.packages) == 0 {
		return false, -1, false // unknown
	}
	for uid, l := range t.uidPkgs {
		for _, p := range l {
			if p == pkg {
				return true, uid, true
			}
		}
	}
	return false, -1, true
}

// Packages returns all known installed packages.
func (t *Tracker) Packages() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.packages))
	for p := range t.packages {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func appID(uid int) int {
	if uid < 0 {
		return uid
	}
	return uid % 100000
}

// packageForLocked is the application id of a *process* (for process lists
// and the process→package mapping). Helper binaries that run with an app's
// uid (crash_dump64, sh, …) are not app processes and get "".
func (t *Tracker) packageForLocked(name string, uid int) string {
	base := name
	if i := strings.IndexByte(base, ':'); i > 0 {
		base = base[:i]
	}
	if name != "" && !strings.Contains(base, ".") {
		return ""
	}
	if a := appID(uid); a >= 10000 {
		if l := t.uidPkgs[a]; len(l) > 0 {
			if len(l) == 1 {
				return l[0]
			}
			for _, p := range l {
				if p == base {
					return p
				}
			}
			return l[0]
		}
		if a >= 90000 { // isolated process: name is usually the package
			return base
		}
	}
	if t.packages[base] {
		return base
	}
	if a := appID(uid); a >= 10000 && strings.Contains(base, ".") {
		return base // AS fallback: process name before ':'
	}
	return ""
}

// Seed reconciles the table with a full `ps` snapshot taken at device time
// atNs: unknown processes are added, placeholder names ("<pre-initialized>")
// are resolved, and live entries missing from the snapshot are closed (they
// died without a lifecycle line we could see, e.g. helper processes).
func (t *Tracker) Seed(ps []PSEntry, atNs int64) {
	t.mu.Lock()
	var changed []Proc
	inPS := map[int]bool{}
	for _, e := range ps {
		inPS[e.PID] = true
		l := t.byPID[e.PID]
		var last *Proc
		if len(l) > 0 {
			last = l[len(l)-1]
		}
		if last != nil && last.Alive() {
			switch {
			case last.Name == e.Name, placeholderName(e.Name):
			case placeholderName(last.Name):
				last.Name = e.Name
				if last.UID < 0 {
					last.UID = e.UID
				}
				last.Package = t.packageForLocked(last.Name, last.UID)
				changed = append(changed, *last)
			case last.StartNs != 0 && last.StartNs > atNs-startSlackNs:
				// started right around the snapshot: trust the lifecycle event
			default:
				// pid reused while we were not looking
				last.EndNs, last.EndReason = atNs, "gone (pid reused by "+e.Name+")"
				changed = append(changed, *last)
				last = nil
			}
			if last != nil {
				continue
			}
		}
		if placeholderName(e.Name) {
			continue // will be named by am_proc_start / the next snapshot
		}
		if last == nil || !last.Alive() {
			if o := t.aliveByNameLocked(e.Name, e.UID, e.PID); o != nil && o.Source != "ps" {
				// Android runs one process per process name. A second pid with the
				// same name and no am_proc_start is the crash handler's short-lived
				// fork of a crashing process (debuggerd), not an app process.
				continue
			}
		}
		if last != nil && last.EndNs > atNs-startSlackNs {
			continue // ended around the snapshot time: ps raced with the death
		}
		p := &Proc{Boot: t.boot, PID: e.PID, UID: e.UID, Name: e.Name, Source: "ps", SeenNs: atNs}
		p.Package = t.packageForLocked(e.Name, e.UID)
		t.upsertLocked(p)
		changed = append(changed, *p)
	}
	// A full snapshot proves absence. Ignore suspiciously short outputs.
	if len(ps) >= 20 && atNs > 0 && t.lastNs >= atNs-psCatchUpNs {
		for pid, l := range t.byPID {
			if inPS[pid] {
				continue
			}
			for _, q := range l {
				if q.Alive() && order(q) < atNs-startSlackNs {
					q.EndNs, q.EndReason = atNs, GoneNotInPS
					changed = append(changed, *q)
				}
			}
		}
	}
	t.mu.Unlock()
	t.emit(changed)
}

func (t *Tracker) aliveByNameLocked(name string, uid, pid int) *Proc {
	for p, l := range t.byPID {
		if p == pid || len(l) == 0 {
			continue
		}
		if q := l[len(l)-1]; q.Alive() && q.Name == name && (uid < 0 || q.UID < 0 || q.UID == uid) {
			return q
		}
	}
	return nil
}

// CloseAll ends every live entry (e.g. the device rebooted) at endNs.
func CloseAll(ps []Proc, endNs int64, reason string) []Proc {
	var out []Proc
	for _, p := range ps {
		if p.Alive() {
			p.EndNs, p.EndReason = endNs, reason
			out = append(out, p)
		}
	}
	return out
}

func (t *Tracker) emit(ps []Proc) {
	if t.OnChange == nil {
		return
	}
	for _, p := range ps {
		t.OnChange(p)
	}
}

// Lookup finds the process that owned pid at time ns.
func (t *Tracker) Lookup(pid int, ns int64) *Proc {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lookupLocked(pid, ns)
}

func (t *Tracker) lookupLocked(pid int, ns int64) *Proc {
	l := t.byPID[pid]
	for i := len(l) - 1; i >= 0; i-- {
		if l[i].StartNs <= ns+startSlackNs {
			if l[i].EndNs > 0 && ns > l[i].EndNs+5e9 {
				return nil // that process was gone by then; the pid is unknown
			}
			return l[i]
		}
	}
	return nil
}

// Annotate fills r.Process / r.Package. It returns false when the pid is
// unknown (caller may schedule a ps refresh).
func (t *Tracker) Annotate(r *model.Record) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r.PID == 0 {
		r.Process = "kernel"
		return true
	}
	if p := t.lookupLocked(r.PID, r.TimeNs); p != nil && !placeholderName(p.Name) {
		r.Process = p.Name
		r.Package = p.Package
		if r.Package == "" && r.UID >= 0 {
			// e.g. crash_dump64 runs with the crashed app's uid: the line
			// belongs to the app (package filter), the process stays crash_dump64
			r.Package = t.packageForLocked("", r.UID)
		}
		return true
	}
	if r.UID >= 0 {
		r.Package = t.packageForLocked("", r.UID)
	}
	switch r.Tag {
	case "DEBUG", "crash_dump64", "crash_dump32":
		r.Process = r.Tag
		if r.Tag == "DEBUG" {
			r.Process = "crash_dump"
		}
		return true
	}
	return false
}

var (
	reStartProc    = regexp.MustCompile(`^Start proc (\d+):([^/\s]+)/(\S+) for `)
	reStartProcOld = regexp.MustCompile(`^Start proc ([^\s]+) for .*?: pid=(\d+) uid=(\d+)`)
	reHasDied      = regexp.MustCompile(`^Process (\S+) \(pid (\d+)\) has died`)
	reKilling      = regexp.MustCompile(`^Killing (\d+):([^/\s]+)/\S+ \(adj -?\d+\): (.*)`)
	reForceStop    = regexp.MustCompile(`^Force stopping (\S+) appid=(\d+)`)
	reZygoteExit   = regexp.MustCompile(`^Process (\d+) exited (?:due to signal (\d+)|cleanly)`)
	reUserUID      = regexp.MustCompile(`^u(\d+)([ai])(\d+)$`)
)

// ParseUserUID turns "u0a123" / "u0_a123" / "1000" / "system" into a uid.
func ParseUserUID(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	s2 := strings.ReplaceAll(s, "_", "")
	if m := reUserUID.FindStringSubmatch(s2); m != nil {
		u, _ := strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[3])
		if m[2] == "a" {
			return u*100000 + 10000 + n
		}
		return u*100000 + 90000 + n
	}
	if v, ok := userNames[s]; ok {
		return v
	}
	return -1
}

var userNames = map[string]int{"root": 0, "system": 1000, "radio": 1001, "bluetooth": 1002, "graphics": 1003, "input": 1004, "audio": 1005, "camera": 1006, "log": 1007, "compass": 1008, "mount": 1009, "wifi": 1010, "media": 1013, "dhcp": 1014, "sdcard_rw": 1015, "vpn": 1016, "keystore": 1017, "nfc": 1027, "shell": 2000, "cache": 2001, "nobody": 9999, "audioserver": 1041, "cameraserver": 1047, "media_codec": 1046, "webview_zygote": 1053, "credstore": 1076, "gpu_service": 1072, "network_stack": 1073, "statsd": 1066, "incidentd": 1067, "secure_element": 1068, "lmkd": 1069, "llkd": 1070, "iorapd": 1071, "nfc_hal": 1074}

// Observe updates the table from lifecycle evidence in r.
func (t *Tracker) Observe(r *model.Record) {
	t.mu.Lock()
	if r.TimeNs > t.lastNs {
		t.lastNs = r.TimeNs
	}
	t.mu.Unlock()
	var started, ended *Proc
	var crashedPID int
	forceStop := ""
	switch {
	case r.Tag == "am_proc_start":
		// [User, PID, UID, Process Name, Type, Component]
		f := logcat.EventFields(r.Msg, 6)
		if len(f) >= 4 {
			pid, e1 := strconv.Atoi(f[1])
			uid, e2 := strconv.Atoi(f[2])
			if e1 == nil && e2 == nil {
				started = &Proc{PID: pid, UID: uid, Name: f[3], StartNs: r.TimeNs, Source: "am_proc_start"}
			}
		}
	case r.Tag == "am_proc_died" || r.Tag == "am_kill":
		// am_proc_died: [User, PID, Process Name, OomAdj, ProcState]; am_kill: [User, PID, Process Name, OomAdj, Reason]
		f := logcat.EventFields(r.Msg, 5)
		if len(f) >= 3 {
			if pid, err := strconv.Atoi(f[1]); err == nil {
				reason := "died"
				if r.Tag == "am_kill" && len(f) >= 5 {
					reason = "killed: " + f[4]
				}
				ended = &Proc{PID: pid, Name: f[2], EndNs: r.TimeNs, EndReason: reason}
			}
		}
	case r.Tag == "am_crash":
		if pid, _, _, ok := AmCrashPID(r.Msg); ok && !strings.Contains(r.Msg, ",Native crash,") {
			crashedPID = pid
		}
	case r.Tag == "Zygote":
		if m := reZygoteExit.FindStringSubmatch(r.Msg); m != nil {
			pid, _ := strconv.Atoi(m[1])
			reason := "exited"
			if m[2] != "" {
				reason = "exited (signal " + m[2] + ")"
			}
			ended = &Proc{PID: pid, EndNs: r.TimeNs, EndReason: reason}
		}
	case r.Tag == "ActivityManager":
		if m := reStartProc.FindStringSubmatch(r.Msg); m != nil {
			pid, _ := strconv.Atoi(m[1])
			started = &Proc{PID: pid, UID: ParseUserUID(m[3]), Name: m[2], StartNs: r.TimeNs, Source: "Start proc"}
		} else if m := reStartProcOld.FindStringSubmatch(r.Msg); m != nil {
			pid, _ := strconv.Atoi(m[2])
			uid, _ := strconv.Atoi(m[3])
			started = &Proc{PID: pid, UID: uid, Name: m[1], StartNs: r.TimeNs, Source: "Start proc"}
		} else if m := reHasDied.FindStringSubmatch(r.Msg); m != nil {
			pid, _ := strconv.Atoi(m[2])
			ended = &Proc{PID: pid, Name: m[1], EndNs: r.TimeNs, EndReason: "died"}
		} else if m := reKilling.FindStringSubmatch(r.Msg); m != nil {
			pid, _ := strconv.Atoi(m[1])
			ended = &Proc{PID: pid, Name: m[2], EndNs: r.TimeNs, EndReason: "killed: " + m[3]}
		} else if m := reForceStop.FindStringSubmatch(r.Msg); m != nil {
			forceStop = m[1]
		}
	case r.Tag == "AndroidRuntime" && strings.HasPrefix(r.Msg, "FATAL EXCEPTION"):
		crashedPID = r.PID
	case r.Tag == "libc" && r.Level.Rank() == 6 && strings.HasPrefix(r.Msg, "Fatal signal"):
		crashedPID = r.PID
	}
	if started == nil && ended == nil && crashedPID == 0 && forceStop == "" {
		return
	}
	var changed []Proc
	t.mu.Lock()
	if started != nil {
		started.Boot = t.boot
		l := t.byPID[started.PID]
		dup := false
		for _, q := range l {
			if q.Name == started.Name && q.StartNs != 0 && abs64(q.StartNs-started.StartNs) < 5e9 {
				dup = true // same start seen via both events and system buffer
				if q.UID < 0 {
					q.UID = started.UID
				}
			}
		}
		if !dup {
			// A live entry that only came from a ps snapshot (taken after the
			// fork, before this line was ingested) is the same process.
			for _, q := range l {
				if q.Alive() && q.StartNs == 0 && (q.Name == started.Name || placeholderName(q.Name)) &&
					q.SeenNs >= started.StartNs-startSlackNs {
					q.StartNs, q.Name, q.Source = started.StartNs, started.Name, started.Source
					if started.UID >= 0 {
						q.UID = started.UID
					}
					q.Package = t.packageForLocked(q.Name, q.UID)
					t.sortLocked(q.PID)
					changed = append(changed, *q)
					dup = true
				}
			}
		}
		if !dup {
			for _, q := range l {
				if q.Alive() {
					q.EndNs = started.StartNs
					q.EndReason = "pid reused"
					changed = append(changed, *q)
				}
			}
			started.Package = t.packageForLocked(started.Name, started.UID)
			t.upsertLocked(started)
			changed = append(changed, *started)
		}
	}
	if ended != nil {
		if p := t.lookupLocked(ended.PID, ended.EndNs); p != nil && (p.Alive() || p.EndReason == GoneNotInPS && ended.EndNs < p.EndNs) {
			p.EndNs = ended.EndNs
			p.EndReason = ended.EndReason
			changed = append(changed, *p)
		}
	}
	if forceStop != "" {
		for _, l := range t.byPID {
			for _, q := range l {
				if q.Alive() && q.Package == forceStop && order(q) < r.TimeNs {
					q.EndNs, q.EndReason = r.TimeNs, "force-stopped"
					changed = append(changed, *q)
				}
			}
		}
	}
	if crashedPID != 0 {
		if p := t.lookupLocked(crashedPID, r.TimeNs); p != nil && !p.Crashed {
			p.Crashed = true
			changed = append(changed, *p)
		}
	}
	t.mu.Unlock()
	t.emit(changed)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// ForPackage returns lifetimes of processes belonging to pkg (exact appId,
// i.e. "com.foo" and "com.foo:remote"), newest first.
func (t *Tracker) ForPackage(pkg string) []Proc {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []Proc
	for _, l := range t.byPID {
		for _, p := range l {
			if p.Package == pkg || p.Name == pkg || strings.HasPrefix(p.Name, pkg+":") {
				out = append(out, *p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alive() != out[j].Alive() {
			return out[i].Alive()
		}
		return out[i].StartNs > out[j].StartNs
	})
	return out
}

// PSEntry is one parsed `ps` row.
type PSEntry struct {
	PID  int
	UID  int
	Name string
}

// ParsePS parses `ps -A -o PID,UID,NAME` or default toybox/legacy `ps` output.
func ParsePS(out string) []PSEntry {
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	if len(lines) == 0 {
		return nil
	}
	hdr := strings.Fields(lines[0])
	col := func(names ...string) int {
		for i, h := range hdr {
			for _, n := range names {
				if strings.EqualFold(h, n) {
					return i
				}
			}
		}
		return -1
	}
	pidC, uidC, userC := col("PID"), col("UID"), col("USER")
	if pidC < 0 {
		return nil
	}
	var res []PSEntry
	for _, ln := range lines[1:] {
		f := strings.Fields(ln)
		if len(f) < len(hdr) || len(f) <= pidC {
			continue
		}
		pid, err := strconv.Atoi(f[pidC])
		if err != nil {
			continue
		}
		uid := -1
		if uidC >= 0 {
			uid = ParseUserUID(f[uidC])
		} else if userC >= 0 {
			uid = ParseUserUID(f[userC])
		}
		// NAME is the last column (may contain spaces in odd cases)
		name := strings.Join(f[len(hdr)-1:], " ")
		if name == "" || name == "ps" || strings.HasPrefix(name, "[") {
			continue
		}
		res = append(res, PSEntry{PID: pid, UID: uid, Name: name})
	}
	return res
}

// ParsePackagesU parses `pm list packages -U` ("package:com.foo uid:10123"
// or "uid:10123,10124" for multi-user) into appId -> packages.
func ParsePackagesU(out string) map[int][]string {
	m := map[int][]string{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "package:") {
			continue
		}
		parts := strings.Fields(ln[len("package:"):])
		if len(parts) == 0 {
			continue
		}
		pkg := parts[0]
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, "uid:") {
				for _, u := range strings.Split(p[4:], ",") {
					if n, err := strconv.Atoi(u); err == nil {
						a := appID(n)
						if !containsStr(m[a], pkg) {
							m[a] = append(m[a], pkg)
						}
					}
				}
			}
		}
		if len(parts) == 1 {
			m[-1] = append(m[-1], pkg)
		}
	}
	for k := range m {
		sort.Strings(m[k])
	}
	return m
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// AmCrashPID extracts the crashed pid and process name from an am_crash
// event. The field order changed across releases ([User,PID,Process,…] in
// AOSP's EventLogTags vs [PID,User,Process,…] observed on API 29), so the pid
// is chosen by value: the user id is small (0, 10, 11 …) while pids are not.
func AmCrashPID(msg string) (pid, user int, proc string, ok bool) {
	f := logcat.EventFields(msg, 4)
	if len(f) < 3 {
		return 0, 0, "", false
	}
	a, e1 := strconv.Atoi(strings.TrimSpace(f[0]))
	b, e2 := strconv.Atoi(strings.TrimSpace(f[1]))
	if e1 != nil || e2 != nil {
		return 0, 0, "", false
	}
	switch {
	case a == 0:
		pid, user = b, a
	case b == 0:
		pid, user = a, b
	case a > b:
		pid, user = a, b
	default:
		pid, user = b, a
	}
	return pid, user, f[2], true
}
