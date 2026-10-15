package device

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Pipeline is the adb-independent ingest path of one boot session:
// record → process tracking → (short delay) → annotate → store → crash
// detection. The live recorder feeds it from `adb logcat`; tests replay
// captured logcat/dropbox/ps output through exactly the same code.
type Pipeline struct {
	Store *store.Store
	Procs *procs.Tracker
	Det   *analysis.Detector
	Boot  string
	Loc   *time.Location
	// Delay lets lifecycle lines that arrive slightly late (events vs main
	// buffer) name a new pid before its first lines are stored.
	Delay time.Duration

	OnUnknownPID func(newestNs int64) // stored lines had an unknown pid; the newest one's time
	NeedDropbox  func(tag string)     // a crash wants DropBox evidence
	OnUnknownUID func(uid int)        // a device process has an app uid missing from the package list
	BeforeAppend func(first *model.Record)
	// DeviceNow returns device now in ns (0 = unknown); it sets the
	// DropBox import cutoff (issue #53). Nil: the newest stored line.
	DeviceNow func() int64

	// Clock jump detection (issue #48). Live: lines arrive as they are
	// logged (catch-up is done), so their host arrival time is meaningful.
	Live         bool
	Clock        *LiveClock // live lines set the clock offset (nil: not tracked)
	OnClockEvent func()     // a clock jump or an auto_time change was seen
	jump         jumpState

	accepted uint64

	queue   []pending
	crashMu sync.Mutex
}

// jumpState follows device time in arrival order for clock jumps.
type jumpState struct {
	prev, max int64 // device time of the previous line; highest since the last back jump
	set       bool  // the previous line announced a clock set
}

// possibleJumpNs: in lines without a usable arrival time (backfill), a step
// of the device time this large between consecutive lines is reported as
// a possible forward jump.
const possibleJumpNs = int64(time.Hour)

// detectJumps checks stored lines (in arrival order) for device clock
// jumps (issue #48). Live lines go to the LiveClock (two-line rule against
// the clock offset). Replayed lines never touch the offset: a set is found
// from the AlarmManagerService "Setting time of day to sec=N" line (exact,
// at the next line), a step back of more than the threshold between
// consecutive lines, or a step forward of more than possibleJumpNs
// (a forward jump or a quiet period).
// It returns the jumps found, in arrival order.
func (p *Pipeline) detectJumps(batch []*model.Record, arrived []time.Time, live, late []bool) []store.ClockJump {
	th := store.ClockJumpThreshold
	event := false
	var found []store.ClockJump
	for i, r := range batch {
		if late[i] {
			continue
		}
		js := &p.jump
		announced := js.set
		_, js.set = clockSetLine(r)
		if r.Tag == "SettingsProvider" && strings.Contains(r.Msg, "settings/global/auto_time") {
			event = true
		}
		var j *store.ClockJump
		switch {
		case live[i] && p.Clock != nil:
			j = p.Clock.Line(r.Seq, r.TimeNs, js.prev, arrived[i], announced)
		case js.prev == 0:
		case announced && (r.TimeNs-js.prev > th || js.prev-r.TimeNs > th):
			j = &store.ClockJump{Seq: r.Seq, DeltaNs: r.TimeNs - js.prev, BeforeNs: js.prev, AfterNs: r.TimeNs, Source: "set"}
		case r.TimeNs < js.max-th:
			// far below the lines before it: the clock was set back
			// (buffers interleave by seconds, a backlog keeps the order)
			j = &store.ClockJump{Seq: r.Seq, DeltaNs: r.TimeNs - js.prev, BeforeNs: js.prev, AfterNs: r.TimeNs, Source: "lines"}
		case r.TimeNs > js.prev+possibleJumpNs:
			j = &store.ClockJump{Seq: r.Seq, DeltaNs: r.TimeNs - js.prev, BeforeNs: js.prev, AfterNs: r.TimeNs, Source: "possible"}
		}
		if j != nil {
			event = true
			found = append(found, *j)
			if j.DeltaNs < 0 {
				js.max = r.TimeNs
			}
			if p.Store.IsWriter() {
				p.Store.AddClockJump(*j)
			}
		}
		js.prev = r.TimeNs
		js.max = max(js.max, r.TimeNs)
	}
	if event && p.OnClockEvent != nil {
		p.OnClockEvent()
	}
	return found
}

// clockSetLine: the framework logs "Setting time of day to sec=N" right
// before it changes the device clock (main buffer; tag AlarmManagerService
// on API 21–33, SystemClockTime on API 34+; a trailing newline on old
// versions), and "Current time only <ms>, advancing to build time <ms>"
// at boot (AlarmManager / SystemClockTime). An optional anchor only:
// `adb shell date` as root sets the clock without logging anything, so
// jumps are detected from the lines' device times (issue #48).
func clockSetLine(r *model.Record) (int64, bool) {
	switch r.Tag {
	case "AlarmManagerService", "SystemClockTime", "AlarmManager":
	default:
		return 0, false
	}
	if m := reClockSet.FindStringSubmatch(r.Msg); m != nil && r.Tag != "AlarmManager" {
		n, err := strconv.ParseInt(m[1], 10, 64)
		return n * 1e9, err == nil
	}
	if m := reClockBoot.FindStringSubmatch(r.Msg); m != nil && r.Tag != "AlarmManagerService" {
		n, err := strconv.ParseInt(m[1], 10, 64)
		return n * 1e6, err == nil
	}
	return 0, false
}

var (
	reClockSet  = regexp.MustCompile(`^Setting time of day to sec=(-?\d+)\s*$`)
	reClockBoot = regexp.MustCompile(`^Current time only -?\d+, advancing to build time (\d+)`)
)

type pending struct {
	r       *model.Record
	arrived time.Time
	live    bool // accepted while Live
	late    bool // recovered from the device buffer after later lines (AcceptLate)
}

// NewPipeline prepares ingestion for boot (short boot id) into st.
func NewPipeline(st *store.Store, boot string, loc *time.Location) *Pipeline {
	if loc == nil {
		loc = time.Local
	}
	p := &Pipeline{Store: st, Procs: procs.New(), Det: analysis.NewDetector(), Boot: boot, Loc: loc, Delay: 800 * time.Millisecond}
	p.Procs.Reset(boot)
	var ps []procs.Proc
	for _, x := range LoadProcs(st) {
		if x.Boot == boot {
			ps = append(ps, x)
		}
	}
	p.Procs.Restore(ps)
	if m := st.Meta(); len(m.Sessions) > 0 && m.Sessions[len(m.Sessions)-1].Boot == boot {
		if n := st.Stats().NewestNs; n != 0 {
			p.jump.prev, p.jump.max = n, n // continue the session's timeline
		}
	}
	// before SetPackages: entries it re-attributes are persisted (issue #41)
	p.Procs.OnChange = func(x procs.Proc) { appendProc(st, x) }
	if pk := LoadPackages(st); len(pk) > 0 {
		p.Procs.SetPackages(pk)
	}
	p.Procs.OnUnknownUID = func(uid int) {
		if f := p.OnUnknownUID; f != nil {
			f(uid)
		}
	}
	p.Det.Emit = p.EmitCrash
	p.Det.PackageOf = p.Procs.OwnerOf // dotted process names (issue #41)
	p.Det.NeedDropbox = func(tag string) {
		if p.NeedDropbox != nil {
			p.NeedDropbox(tag)
		}
	}
	return p
}

// Accept takes one assembled record (already deduplicated by the caller).
func (p *Pipeline) Accept(r *model.Record, arrived time.Time) {
	if r == nil {
		return
	}
	r.Boot = p.Boot
	p.Procs.Observe(r)
	p.queue = append(p.queue, pending{r, arrived, p.Live, false})
	p.accepted++
}

// AcceptLate takes a record the live stream skipped and a later dump of
// the device buffer recovered (issue #65). It is stored like any other,
// but its device time says nothing about clock jumps: lines around it
// arrived long before.
func (p *Pipeline) AcceptLate(r *model.Record, arrived time.Time) {
	if r == nil {
		return
	}
	r.Boot = p.Boot
	p.Procs.Observe(r)
	p.queue = append(p.queue, pending{r, arrived, false, true})
	p.accepted++
}

// Accepted / Appended count the records accepted and appended to the
// store so far (catch-up: the line at T_end is stored once Appended
// reaches the Accepted count at the time it was accepted).
func (p *Pipeline) Accepted() uint64 { return p.accepted }
func (p *Pipeline) Appended() uint64 { return p.accepted - uint64(len(p.queue)) }

// Pending reports queued records.
func (p *Pipeline) Pending() int { return len(p.queue) }

// Drain stores queued records older than Delay (all: everything).
func (p *Pipeline) Drain(all bool, now time.Time) error {
	cut := now.Add(-p.Delay)
	n := 0
	for n < len(p.queue) && (all || p.queue[n].arrived.Before(cut) || len(p.queue)-n > 5000) {
		n++
	}
	if n == 0 {
		return nil
	}
	batch := make([]*model.Record, n)
	arrived := make([]time.Time, n)
	live := make([]bool, n)
	late := make([]bool, n)
	unknown := false
	var unknownNs int64
	for i := 0; i < n; i++ {
		r := p.queue[i].r
		if !p.Procs.Annotate(r) && r.PID > 0 {
			unknown = true
			unknownNs = max(unknownNs, r.TimeNs)
		}
		batch[i], arrived[i], live[i], late[i] = r, p.queue[i].arrived, p.queue[i].live, p.queue[i].late
	}
	p.queue = append(p.queue[:0], p.queue[n:]...)
	if p.BeforeAppend != nil {
		p.BeforeAppend(batch[0])
	}
	if err := p.Store.Append(batch); err != nil {
		return err
	}
	jumps := p.detectJumps(batch, arrived, live, late)
	for _, r := range batch {
		// the detector sees each clock set before the first line that
		// carries the new clock (issue #55); a "possible" jump is a quiet
		// period, not a clock change
		for len(jumps) > 0 && jumps[0].Seq <= r.Seq {
			if jumps[0].Source != "possible" {
				p.Det.ClockJump(jumps[0].DeltaNs)
			}
			jumps = jumps[1:]
		}
		p.Det.Feed(r)
	}
	if unknown && p.OnUnknownPID != nil {
		p.OnUnknownPID(unknownNs)
	}
	return nil
}

// Tick finalizes quiet native crashes (deviceNs 0: newest record time).
func (p *Pipeline) Tick(deviceNs int64) { p.Det.Tick(deviceNs) }

// EmitCrash assigns an ID (crN / anrN) to new crashes and persists the
// current version (later lines with the same ID supersede earlier ones).
func (p *Pipeline) EmitCrash(c *analysis.Crash) {
	p.crashMu.Lock()
	defer p.crashMu.Unlock()
	if c.ID == "" && !strings.HasPrefix(firstSource(c), "dropbox:") {
		// pick the DropBox-only crash to adopt from the summaries, then
		// adopt from its full version (one decode, not the whole file)
		probe := *c
		if analysis.AdoptDropbox(CrashSummaries(p.Store), &probe) {
			if full := CrashFull(p.Store, probe.ID)[0]; full != nil {
				analysis.AdoptDropbox([]*analysis.Crash{full}, c)
			}
		}
	} else if c.ID != "" {
		if old := CrashFull(p.Store, c.ID)[0]; old != nil {
			analysis.KeepDropbox(c, old)
		}
	}
	if c.ID == "" {
		p.Store.UpdateMeta(func(m *store.Meta) {
			if c.Kind == "anr" {
				m.AnrSeq++
				c.ID = "anr" + strconv.Itoa(m.AnrSeq)
			} else {
				m.CrashSeq++
				c.ID = "cr" + strconv.Itoa(m.CrashSeq)
			}
		})
	}
	p.Store.AppendAux("crashes", crashAuxLine{Crash: c, HostNs: time.Now().UnixNano()})
}

// ApplyDropbox merges `dumpsys dropbox --print <tag>` output into the crash list.
func (p *Pipeline) ApplyDropbox(out string) {
	seen := map[string]bool{}
	for _, b := range p.Store.ReadAux("dropbox_seen") {
		if k, _, ok := parseSeen(b); ok {
			seen[k] = true
		}
	}
	entries := analysis.ParseDropbox(out, p.Loc)
	// Entries older than the retention window are not imported: their
	// dropbox_seen keys may already have been cleaned up (the device keeps
	// DropBox for ~3 days, the default retention is 1 day), and re-importing
	// them would show old crashes as new. DropBox stamps entries with the
	// device clock, so the cutoff is device now − retention, not the host
	// clock and not the highest device time ever stored, which a clock
	// jump leaves far ahead (issue #53). Entries dated after device now
	// (written before the clock was set back) are kept out of a second
	// import by their dropbox_seen keys.
	var cutoff int64
	now := int64(0)
	if p.DeviceNow != nil {
		now = p.DeviceNow()
	}
	if now == 0 {
		now = p.Store.Stats().NewestNs // newest arrival
	}
	if now != 0 {
		cutoff = now - int64(p.Store.Config().MaxAge)
	}
	for _, e := range entries {
		if e.TimeNs != 0 && e.TimeNs < cutoff {
			continue
		}
		// Keyed by content when the body names the crashed pid (a pid
		// crashes once, so the body is unique): DropBox re-times entries
		// dated after device now when the clock was set back, and a time
		// key would import the entry again (issue #63). Time keys of
		// older versions are still honoured.
		key := e.Tag + "|" + strconv.FormatInt(e.TimeNs, 10)
		if seen[key] {
			continue
		}
		if e.Note == "" && namesPid(e.Body) {
			key = dropboxBodyKey(e)
			if seen[key] {
				continue
			}
		}
		// match against the summaries; for an existing crash, merge into
		// its full version (MatchDropbox copies the match and keeps its
		// traces / DropBox text)
		c, isNew := analysis.MatchDropboxBoot(CrashSummaries(p.Store), e, p.Boot)
		if c != nil && !isNew {
			full := CrashFull(p.Store, c.ID)[0]
			if full == nil {
				continue
			}
			c, isNew = analysis.MatchDropboxBoot([]*analysis.Crash{full}, e, p.Boot)
		}
		p.Store.AppendAux("dropbox_seen", seenLine{Key: key, HostNs: time.Now().UnixNano()})
		if c == nil {
			continue
		}
		if isNew {
			c.ID = ""
			if c.Boot == "" {
				c.Boot = p.Boot
			}
		}
		if c.Process != "" && (c.Package == "" || c.Package == analysis.PackageOfName(c.Process)) {
			c.Package = p.Procs.OwnerOf(c.Process) // dotted process names (issue #41)
		}
		p.EmitCrash(c)
	}
}

// SeedPS reconciles processes with `ps -A -o PID,UID,NAME` output taken at device time atNs.
func (p *Pipeline) SeedPS(out string, atNs int64) {
	if e := procs.ParsePS(out); len(e) > 0 {
		p.Procs.Seed(e, atNs)
	}
}

// ReplayText feeds raw `logcat -v threadtime -v epoch -v uid -v usec
// -v printable -D` text through the pipeline as if it arrived live (arrival
// time = record time), then stores everything and finalizes crashes.
func (p *Pipeline) ReplayText(text string) error {
	asm := &logcat.Assembler{Loc: p.Loc, Year: time.Now().Year()}
	var clock time.Time
	feed := func(r *model.Record) error {
		if r == nil {
			return nil
		}
		if t := time.Unix(0, r.TimeNs); t.After(clock) {
			clock = t
		}
		p.Accept(r, clock)
		return p.Drain(false, clock)
	}
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if err := feed(asm.Push(ln)); err != nil {
			return err
		}
	}
	if err := feed(asm.Flush()); err != nil {
		return err
	}
	if err := p.Drain(true, clock); err != nil {
		return err
	}
	p.Tick(clock.Add(time.Minute).UnixNano())
	p.Store.Flush()
	return nil
}

// LoadProcs reads process lifetimes (last version of each entry wins).
func LoadProcs(st *store.Store) []procs.Proc {
	m := map[string]procs.Proc{}
	var order []string
	for _, b := range st.ReadAux("procs") {
		var p procs.Proc
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		k := p.ID
		if k == "" { // pre-ID files
			k = fmt.Sprintf("%s|%d|%d|%s", p.Boot, p.PID, p.StartNs, p.Name)
		}
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = p
	}
	out := make([]procs.Proc, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

// LoadPackages reads the shared uid → packages cache.
func LoadPackages(st *store.Store) map[int][]string {
	b, err := os.ReadFile(filepath.Join(st.Dir, "packages.json"))
	if err != nil {
		return nil
	}
	var m map[int][]string
	json.Unmarshal(b, &m)
	return m
}

// WritePackages stores the uid → packages cache.
func WritePackages(st *store.Store, m map[int][]string) {
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(st.Dir, "packages.json"), b, 0o644)
}

// CloseOtherBoots ends process entries of earlier boots that never got an
// end (the device rebooted under them). endNs is the last record time of
// that boot, if known.
func CloseOtherBoots(st *store.Store, boot string) {
	var open []procs.Proc
	for _, p := range LoadProcs(st) {
		if p.Boot != boot && p.Alive() {
			open = append(open, p)
		}
	}
	for _, p := range open {
		e := BootEndNs(st, p.Boot)
		if e <= 0 {
			e = max(p.StartNs, p.SeenNs)
		}
		p.EndNs, p.EndReason = e, "device rebooted"
		appendProc(st, p)
	}
}

// BootEndNs is the time of the last stored record of a boot session (0 if unknown).
func BootEndNs(st *store.Store, boot string) int64 {
	m := st.Meta()
	for i, s := range m.Sessions {
		if s.Boot != boot {
			continue
		}
		last := st.LastSeq()
		if i+1 < len(m.Sessions) {
			last = m.Sessions[i+1].FirstSeq - 1
		}
		var t int64
		st.Scan(0, last+1, true, func(r *model.Record) bool {
			if r.Boot == boot {
				t = r.TimeNs
			}
			return false
		})
		return t
	}
	return 0
}

func firstSource(c *analysis.Crash) string {
	if len(c.Sources) == 0 {
		return ""
	}
	return c.Sources[0]
}

// dropboxBodyKey is the dropbox_seen key of an entry with contents:
// "tag|b:<FNV-64a of the body, hex>". Stable when DropBox re-times the
// entry (issue #63).
func dropboxBodyKey(e analysis.DropboxEntry) string {
	h := fnv.New64a()
	h.Write([]byte(e.Body))
	return e.Tag + "|b:" + strconv.FormatUint(h.Sum64(), 16)
}

// namesPid reports whether a DropBox body names the crashed pid ("PID: n"
// in crash and ANR headers, "pid: n," in tombstones).
func namesPid(body string) bool {
	return strings.Contains(body, "\nPID: ") || strings.HasPrefix(body, "PID: ") || strings.Contains(body, "pid: ")
}
