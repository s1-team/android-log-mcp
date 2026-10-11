package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	accepted uint64

	queue   []pending
	crashMu sync.Mutex
}

type pending struct {
	r       *model.Record
	arrived time.Time
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
	// before SetPackages: entries it re-attributes are persisted (issue #41)
	p.Procs.OnChange = func(x procs.Proc) { st.AppendAux("procs", x) }
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
	p.queue = append(p.queue, pending{r, arrived})
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
	unknown := false
	var unknownNs int64
	for i := 0; i < n; i++ {
		r := p.queue[i].r
		if !p.Procs.Annotate(r) && r.PID > 0 {
			unknown = true
			unknownNs = max(unknownNs, r.TimeNs)
		}
		batch[i] = r
	}
	p.queue = append(p.queue[:0], p.queue[n:]...)
	if p.BeforeAppend != nil {
		p.BeforeAppend(batch[0])
	}
	if err := p.Store.Append(batch); err != nil {
		return err
	}
	for _, r := range batch {
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
		analysis.AdoptDropbox(LoadCrashes(p.Store), c)
	} else if c.ID != "" {
		for _, old := range LoadCrashes(p.Store) {
			if old.ID == c.ID {
				analysis.KeepDropbox(c, old)
				break
			}
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
	p.Store.AppendAux("crashes", c)
}

// ApplyDropbox merges `dumpsys dropbox --print <tag>` output into the crash list.
func (p *Pipeline) ApplyDropbox(out string) {
	seen := map[string]bool{}
	for _, b := range p.Store.ReadAux("dropbox_seen") {
		seen[string(b)] = true
	}
	entries := analysis.ParseDropbox(out, p.Loc)
	p.crashMu.Lock()
	list := LoadCrashes(p.Store)
	p.crashMu.Unlock()
	// Entries older than the retention window are not imported: their
	// dropbox_seen keys may already have been cleaned up (the device keeps
	// DropBox for ~3 days, the default retention is 1 day), and re-importing
	// them would show old crashes as new. The cutoff is relative to the
	// newest stored line (device clock), not the host clock.
	var cutoff int64
	if last := p.Store.Meta().LastTimeNs; last > 0 {
		cutoff = last - int64(p.Store.Config().MaxAge)
	}
	for _, e := range entries {
		if e.TimeNs != 0 && e.TimeNs < cutoff {
			continue
		}
		key := fmt.Sprintf("%q", e.Tag+"|"+strconv.FormatInt(e.TimeNs, 10))
		if seen[key] {
			continue
		}
		c, isNew := analysis.MatchDropbox(list, e)
		p.Store.AppendAux("dropbox_seen", json.RawMessage(key))
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
		if isNew {
			list = append(list, c)
		} else {
			for i := range list {
				if list[i].ID == c.ID {
					list[i] = c
				}
			}
		}
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

// LoadCrashes reads all crashes (last version of each ID wins), newest first.
func LoadCrashes(st *store.Store) []*analysis.Crash {
	byID := map[string]*analysis.Crash{}
	var order []string
	for _, b := range st.ReadAux("crashes") {
		c := new(analysis.Crash)
		if json.Unmarshal(b, c) != nil || c.ID == "" {
			continue
		}
		if _, ok := byID[c.ID]; !ok {
			order = append(order, c.ID)
		}
		byID[c.ID] = c
	}
	out := make([]*analysis.Crash, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeNs > out[j].TimeNs })
	return out
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
		st.AppendAux("procs", p)
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
