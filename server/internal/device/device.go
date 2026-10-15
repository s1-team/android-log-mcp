package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Device is one device: its store plus (when this process owns it and the
// device is online) a running recorder. Serial is the stable device ID (the
// key of tools and cursors); Addr is the adb serial it is reached through.
type Device struct {
	Serial string
	Store  *store.Store
	adb    *ADB

	addr   string
	owned  bool   // this process (the service) records the device
	idNote string // why the ID was disambiguated (collision), if it was

	mu           sync.RWMutex
	state        string // adb state ("" = not attached)
	recording    bool
	lastErr      string
	loc          *time.Location
	tzName       string
	lc           LiveClock // clock offset: connect date read + live lines (issue #48)
	clockSaved   time.Time // host time of the offset last written to meta
	autoTime     string    // settings global auto_time ("" unknown), read lazily
	autoStale    bool      // a clock jump / auto_time change was logged since
	autoErr      string    // why the last auto_time read failed
	bootID       string
	pipe         *Pipeline // current boot session (writer only)
	cancel       context.CancelFunc
	lastSeen     time.Time
	unknownPID   time.Time // debounce for ps refresh
	pkgRefreshAt time.Time // last package list read (maybeRefreshPackages)
	pkgRefresh   time.Time
	dbxPending   map[string]int  // DropBox tag → dbxQueued/dbxRunning/dbxAgain (absent: idle)
	dbxDefer     map[string]bool // tags crashes asked for before side work started
	sideOn       bool            // side work of the current session runs (under dbxMu)
	dbxMu        sync.Mutex
	crashMu      sync.Mutex // serializes ID assignment + persistence
	disk         *store.DiskGuard
	runWG        sync.WaitGroup  // the recorder goroutines (run), incl. one still stopping
	sessSeq      uint64          // number of the last recording session started
	sessDone     chan struct{}   // closed when the last session's goroutine returned
	handoff      string          // the last session start that did not wait for the previous one (HandoffNote)
	bg           workers         // background work of the recording sessions (issue #24)
	kids         children        // adb processes started under the session context
	sideCtx      context.Context // context of the session whose side work runs (under dbxMu)
	spanNs       atomic.Int64    // replay span of the device's main log buffer (measureSpan)

	catch           catchState   // backfill / catch-up of the current recording session
	devNowNs        int64        // device clock read by the last probe (0 = unknown)
	devNowCoarse    bool         // devNowNs has whole-second precision only
	rollback        string       // last clock rollback (RollbackNote)
	preProbe        *Probe       // probe that came with the identity probe (used once)
	execOut         atomic.Int32 // 0 unknown, 1 `adb exec-out` works, -1 unsupported (stream with `adb logcat`)
	rollbackSkipped atomic.Int64 // lines dropped as already stored after it
	recovered       atomic.Int64 // lines the live stream skipped, stored late (reconcileLoop, issue #65)
	repairSince     int64        // host ns the repair mode started (0: off; under mu)
}

// errExecOut ends a recording session whose `adb exec-out logcat` failed
// before any line (adbd without exec support): it restarts with `adb logcat`.
var errExecOut = errors.New("adb exec-out is not supported by the device; streaming with adb logcat")

// errDiskPaused ends a recording session because free disk space is low.
var errDiskPaused = errors.New("recording paused: low free disk space")

// IdleGapReason is the gap recorded when recording resumes with -T after
// it was not running (idle, stopped, crashed); it is dropped again when the
// device buffer still held the last stored line (nothing lost).
const IdleGapReason = "recording was idle (no log tool call or pin) or not running; resumed with logcat -T, so only lines the device buffer dropped meanwhile are lost"

// PauseGapReason is the gap recorded while recording is paused for disk space.
const PauseGapReason = "recording paused: free disk space was low (ANDROID_LOG_MCP_MIN_FREE_MB); resumed with logcat -T, so only lines the device buffer dropped meanwhile are lost"

// recordPause notes an open gap (closed by the first record after resuming).
func recordPause(st *store.Store) {
	st.UpdateMeta(func(m *store.Meta) {
		for _, g := range m.Gaps {
			if g.ToNs <= 0 {
				return // already open
			}
		}
		m.Gaps = append(m.Gaps, store.Gap{FromNs: m.ArrivalNs(), ToNs: -1, Reason: PauseGapReason})
	})
}

func newDevice(adb *ADB, st *store.Store, serial string) *Device {
	d := &Device{Serial: serial, Store: st, adb: adb, dbxPending: map[string]int{}, dbxDefer: map[string]bool{}}
	m := st.Meta()
	d.addr = m.Addr
	if d.addr == "" {
		d.addr = serial
	}
	d.applyTZ(m.TZ, m.TZOffset)
	if m.ClockHostNs > 0 {
		d.lc.Set(m.ClockOffsetNs, time.Unix(0, m.ClockHostNs), "date")
	}
	d.autoTime = m.AutoTime
	d.bootID = m.LastBootID
	return d
}

func (d *Device) applyTZ(name, zoneOffset string) {
	loc, desc := ResolveZone(name, zoneOffset, time.Now())
	d.mu.Lock()
	defer d.mu.Unlock()
	if loc == nil {
		if d.loc == nil {
			d.loc, d.tzName = time.Local, "host-local (device zone unknown)"
		}
		return
	}
	d.loc, d.tzName = loc, desc
}

// ResolveZone picks the device zone. `date +%z` (the offset the device
// actually uses) is the source of truth; persist.sys.timezone is only used
// as a name when it agrees with that offset (emulators may pick another
// zone with the same offset, e.g. Asia/Kuching for +0800 — harmless; a
// disagreeing name is ignored).
func ResolveZone(name, zoneOffset string, at time.Time) (*time.Location, string) {
	var named *time.Location
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			named = l
		}
	}
	off, haveOff := 0, false
	if zoneOffset != "" { // "+0800"
		if t, err := time.Parse("-0700", zoneOffset); err == nil {
			_, off = t.Zone()
			haveOff = true
		}
	}
	switch {
	case named != nil && (!haveOff || zoneOffsetAt(named, at) == off):
		return named, name
	case haveOff:
		label := "UTC" + zoneOffset
		if named != nil {
			label += " (persist.sys.timezone=" + name + " disagrees with date +%z; using the offset)"
		}
		return time.FixedZone("UTC"+zoneOffset, off), label
	}
	return nil, ""
}

func zoneOffsetAt(l *time.Location, at time.Time) int {
	_, o := at.In(l).Zone()
	return o
}

// Loc is the device zone (best known).
func (d *Device) Loc() *time.Location {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.loc
}

// TZName is the IANA name or a description.
func (d *Device) TZName() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.tzName
}

// Status snapshot.
type Status struct {
	State     string
	Recording bool
	Writer    bool
	LastErr   string
	WriterPID string
	Paused    string // non-empty: this process would record but disk space is low
	// Owned: this process (the service) records the device; false for
	// devices only on disk and devices over the recording limit.
	Owned bool
	// IDNote explains a disambiguated device ID (ID collision).
	IDNote string
}

func (d *Device) Status() Status {
	d.mu.RLock()
	s := Status{State: d.state, Recording: d.recording, LastErr: d.lastErr, Owned: d.owned, IDNote: d.idNote}
	d.mu.RUnlock()
	s.Writer = d.Store.IsWriter()
	if s.Writer && s.State == "device" {
		s.Paused = d.disk.Reason()
	}
	if !s.Writer {
		if b, err := os.ReadFile(filepath.Join(d.Store.Dir, "writer.lock")); err == nil {
			s.WriterPID = strings.TrimSpace(string(b))
		}
	}
	return s
}

// Addr is the adb serial the device is reached through.
func (d *Device) Addr() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.addr
}

func (d *Device) setAddr(addr string) {
	d.mu.Lock()
	changed := d.addr != addr
	d.addr = addr
	d.mu.Unlock()
	if changed && d.Store.IsWriter() {
		d.Store.UpdateMeta(func(m *store.Meta) { m.Addr = addr })
	}
}

// Owned reports whether this process records the device.
func (d *Device) Owned() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.owned
}

// Matches reports whether name refers to this device: its ID or its adb
// serial (cursors and device= may carry either).
func (d *Device) Matches(name string) bool {
	return name != "" && (name == d.Serial || name == d.Addr())
}

func (d *Device) setErr(err error) {
	d.mu.Lock()
	if err == nil {
		d.lastErr = ""
	} else {
		d.lastErr = err.Error()
	}
	d.mu.Unlock()
}

// setState is called by the manager on track-devices changes.
func (d *Device) setState(ctx context.Context, state string) {
	d.mu.Lock()
	prev := d.state
	d.state = state
	if state != "" {
		d.lastSeen = time.Now()
	}
	running := d.cancel != nil
	owned := d.owned
	d.mu.Unlock()
	if state == "device" && !running && owned {
		d.mu.Lock()
		d.sessSeq++
		sess, prevDone := d.sessSeq, d.sessDone
		done := make(chan struct{})
		d.sessDone = done
		d.mu.Unlock()
		d.catchStart(sess) // a call right after attach waits for this session's catch-up
		cctx, cancel := context.WithCancel(withSession(withChildren(ctx, &d.kids), sess))
		d.mu.Lock()
		d.cancel = cancel
		d.runWG.Add(1)
		d.mu.Unlock()
		go func() {
			defer d.runWG.Done()
			defer close(done)
			defer d.catchDone(sess) // nothing is backfilling any more
			defer func() {
				if h := sessionEndHook.Load(); h != nil {
					(*h)(d, sess)
				}
			}()
			d.handOver(cctx, sess, prevDone)
			if cctx.Err() == nil {
				d.run(cctx)
			}
		}()
	} else if state != "device" && running {
		d.stop()
	}
	if prev != state {
		d.Store.UpdateMeta(func(m *store.Meta) { m.State = state })
	}
}

func (d *Device) stop() {
	d.mu.Lock()
	c := d.cancel
	d.cancel = nil
	d.mu.Unlock()
	if c != nil {
		c()
	}
	d.catchStop()
}

// recorderStopWait bounds the wait for a stopped recorder goroutine (the
// last drain into the store, reaping logcat): release() and the start of
// the next recording session (handOver).
var recorderStopWait = newDur(10 * time.Second)

// sessionEndHook (tests only) runs when a recording session's goroutine
// is about to return, e.g. to hold a stopping recorder.
var sessionEndHook atomic.Pointer[func(*Device, uint64)]

type sessionKey struct{}

// withSession marks ctx as the context of recording session sess.
func withSession(ctx context.Context, sess uint64) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// sessionOf is the recording session of ctx (0 outside of one).
func sessionOf(ctx context.Context) uint64 {
	s, _ := ctx.Value(sessionKey{}).(uint64)
	return s
}

// handOver runs before recording session sess starts (issue #30): a quick
// disconnect/reconnect must not overlap the previous session, whose
// recorder may still be draining into the store and whose background work
// may still be returning. It waits (bounded) for the previous session's
// goroutine (prev, recorderStopWait), then for the background work (the
// #24 wait: bgStopWait, then the adb children are killed with their
// trees). When a wait times out, the session starts anyway; the service
// log and logcat_status (HandoffNote) say so.
func (d *Device) handOver(ctx context.Context, sess uint64, prev <-chan struct{}) {
	var problems []string
	if prev != nil {
		w := recorderStopWait.Get()
		t := time.NewTimer(w)
		select {
		case <-prev:
		case <-t.C:
			killed := d.kids.killAll()
			log.Printf("%s: recording session %d: the recorder of session %d did not stop within %s; starting anyway; killed its adb processes: %s",
				d.Serial, sess, sess-1, w, orNone(killed))
			problems = append(problems, fmt.Sprintf("the recorder of session %d did not stop within %s (its adb children were killed)", sess-1, w))
		}
		t.Stop()
	}
	// stopped again meanwhile: the next session (or stop) waits instead
	if ctx.Err() == nil {
		if names := d.waitBackground(); len(names) > 0 {
			log.Printf("%s: recording session %d: starting although background work of an earlier session did not stop within %s: %s",
				d.Serial, sess, bgStopWait.Get(), strings.Join(names, ", "))
			problems = append(problems, fmt.Sprintf("background work of an earlier session did not stop within %s (%s; its adb children were killed)", bgStopWait.Get(), strings.Join(names, ", ")))
		}
	}
	if prev == nil && len(problems) == 0 {
		return
	}
	note := ""
	if len(problems) > 0 {
		note = fmt.Sprintf("recording session %d started at %s (device time) before session %d had stopped: %s; both may have written this device's store briefly (see the service log)",
			sess, d.Now().Format("2006-01-02 15:04:05"), sess-1, strings.Join(problems, "; "))
	}
	d.mu.Lock()
	d.handoff = note // a clean handover clears an older note
	d.mu.Unlock()
}

// HandoffNote describes the last recording session that started before
// the previous one had stopped (empty if none, or a later session started
// cleanly).
func (d *Device) HandoffNote() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.handoff
}

// probe reads device properties, boot id and clock offset (read-only):
// the probe that came with the identity probe when it is recent (one adb
// shell less when recording starts), else one adb shell.
func (d *Device) probe(ctx context.Context) error {
	d.mu.Lock()
	p := d.preProbe
	d.preProbe = nil
	d.mu.Unlock()
	reused := true
	if p == nil || time.Since(p.T1) > probeReuse.Get() || p.Props["boot_id"] == "" {
		reused = false
		t0 := time.Now()
		out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, probeCmd(false))
		t1 := time.Now()
		if err != nil {
			return err
		}
		p = &Probe{Props: ParseIdentityProps(out), T0: t0, T1: t1}
		if p.Props["boot_id"] == "" && p.Props["now"] == "" {
			return fmt.Errorf("unexpected output %q", firstLines(strings.TrimSpace(out), 3))
		}
	}
	pr := p.Props
	tz, zone := pr["persist.sys.timezone"], pr["zone"]
	d.applyTZ(tz, zone)
	if now, coarse, mid, ok := p.Clock(); ok {
		if reused {
			// the saved probe is up to probeReuse old: its device clock
			// reading would be stale (rollback check, liveFrom; issue #15)
			now = now.Add(time.Since(mid))
			mid = time.Now()
		}
		est := now
		if coarse {
			// date +%s truncates: add 0.5 s to center the estimate.
			est = now.Add(500 * time.Millisecond)
		}
		d.observeDate(est.Sub(mid), mid)
		d.mu.Lock()
		d.devNowNs, d.devNowCoarse = now.UnixNano(), coarse
		d.mu.Unlock()
	} else {
		d.mu.Lock()
		d.devNowNs = 0
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.bootID = pr["boot_id"]
	d.mu.Unlock()
	d.Store.UpdateMeta(func(m *store.Meta) {
		m.Model, m.Release, m.SDK, m.TZ, m.TZOffset = pr["ro.product.model"], pr["ro.build.version.release"], pr["ro.build.version.sdk"], tz, zone
	})
	return nil
}

// run keeps logcat running while the device is online.
func (d *Device) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		d.mu.RLock()
		mayRecord := d.owned
		d.mu.RUnlock()
		// only devices this service attached are recorded (never one over
		// the recording limit); writer.lock still guards against another
		// process writing the directory (e.g. an older version)
		if !mayRecord || (!d.Store.IsWriter() && !d.Store.TryPromote()) {
			d.mu.Lock()
			d.recording = false
			d.mu.Unlock()
			sleep(ctx, 5*time.Second)
			continue
		}
		os.WriteFile(filepath.Join(d.Store.Dir, "writer.lock"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		if d.disk.Paused() {
			// keep the writer lock (one recorder) but do not write; the
			// manager re-checks free space every 10 s
			sleep(ctx, 5*time.Second)
			continue
		}
		started := time.Now()
		err := d.recordOnce(ctx)
		d.mu.Lock()
		d.recording = false
		d.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errDiskPaused) || errors.Is(err, errExecOut) {
			d.setErr(nil)
			continue
		}
		d.setErr(err)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, 15*time.Second)
	}
}

func recHash(r *model.Record) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d|%c|%s|%s", r.TimeNs, r.PID, r.TID, r.Level, r.Tag, r.Msg)
	return h.Sum64()
}

func (d *Device) pipeline() *Pipeline {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.pipe
}

func (d *Device) recordOnce(ctx context.Context) error {
	sess := sessionOf(ctx)
	if err := d.probe(ctx); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	d.mu.RLock()
	bootID := d.bootID
	loc := d.loc
	devNow, coarse := d.devNowNs, d.devNowCoarse
	d.mu.RUnlock()
	boot := store.BootShort(bootID)
	meta := d.Store.Meta()
	newBoot := meta.LastBootID != bootID
	d.catchBegin(sess)
	d.setSideWork(false)

	// T_end: the newest buffered line (by arrival) now, at µs; the replay
	// ends at the streamed line with that device time
	end, terr := d.measureEnd(ctx, loc)
	if terr != nil && ctx.Err() == nil {
		end, terr = d.measureEnd(ctx, loc)
	}
	tEnd := end.k.ns
	resume := ""
	// stored lines the replayed part of the stream can repeat (issue #52):
	// nil on a new boot; whole = read the whole buffer, not -T
	var known multiset
	whole := false
	arrNs := meta.ArrivalNs()
	sameBoot := meta.LastBootID == bootID && arrNs > 0
	tail := 0
	if sameBoot && devNow != 0 {
		limit := devNow
		if coarse {
			limit += int64(time.Second)
		}
		if arrNs > limit {
			// -T <newest arrival> would name a future device time and
			// print nothing until then (issue #11 §4)
			d.noteRollback(arrNs, devNow, loc)
			whole = true
		}
	}
	if sameBoot && !whole && terr == nil && tEnd != 0 && tEnd < arrNs {
		// the newest buffered line is dated before the newest stored one
		// although it arrived later: the device clock went back
		d.noteWhole(fmt.Sprintf("device clock went back: the newest buffered line (device time %s) is earlier than the newest stored line (%s); read the whole buffer without -T and dropped lines already stored",
			time.Unix(0, tEnd).In(loc).Format("2006-01-02 15:04:05.000"), time.Unix(0, arrNs).In(loc).Format("2006-01-02 15:04:05.000")))
		whole = true
	}
	if sameBoot && jumpInTail(d.Store, sessionFirst(d.Store)) {
		// a clock jump in this boot session: is it among the stored
		// lines that may still be in the buffer, or measured now?
		tail = tailSize(d.bufferLines(ctx))
		from := sessionFirst(d.Store)
		if last := d.Store.LastSeq(); last > uint64(tail) && last-uint64(tail) > from {
			from = last - uint64(tail)
		}
		if !whole && jumpInTail(d.Store, from) {
			// device time does not follow arrival order there: -T could
			// skip lines dated before the resume point (issue #52 D1)
			d.noteWhole("device clock jump among the stored lines that may still be in the device buffer: read the whole buffer without -T and dropped lines already stored")
			whole = true
		}
	}
	if whole {
		// any stored line of the session may still be in the buffer: one
		// buffer (system) can hold boot lines while another (main) rotated
		// many times, so the stored lines it spans are not bounded by its
		// entries (issue #75)
		known = tailMultiset(d.Store, maxResumeHashes)
	} else if sameBoot {
		resume = logcat.FormatResumeTime(arrNs)
		known = boundaryMultiset(d.Store, arrNs)
	}
	if rd := meta.ResumeDedup; rd != nil {
		// stored in the previous root (set-root, issue #56): used by every
		// resume until the boot changes or enough lines are stored here
		if rd.Active(bootID, arrNs, d.Store.LastSeq()) {
			if known != nil {
				for _, h := range d.Store.LoadResumeDedup() {
					known[h]++
				}
			}
		} else {
			d.Store.ClearResumeDedup()
		}
	}
	if !sameBoot {
		// new boot (or first contact): import the device's whole ring buffer
		now := time.Now().UnixNano()
		nextSeq := d.Store.LastSeq() + 1 // not inside UpdateMeta: store lock is held there
		d.Store.UpdateMeta(func(m *store.Meta) {
			if m.LastBootID != "" && m.LastBootID != bootID {
				m.Gaps = append(m.Gaps, store.Gap{FromNs: m.ArrivalNs(), ToNs: 0, Reason: "device rebooted (logs from before the reboot that were not yet recorded are lost)"})
			}
			m.Sessions = append(m.Sessions, store.Session{BootID: bootID, Boot: boot, FirstSeq: nextSeq})
			m.LastBootID = bootID
			if m.RecordingSince == 0 {
				m.RecordingSince = now
			}
		})
		// processes of earlier boots are certainly gone now
		CloseOtherBoots(d.Store, boot)
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case terr != nil:
		log.Printf("%s: measuring the end of the device buffer failed (%v); catch-up is not verified", d.Serial, terr)
		d.catchSetEnd(sess, 0, "could not measure the end of the device buffer: "+terr.Error())
		d.catchDone(sess)
	case tEnd == 0:
		d.catchDone(sess) // empty buffer
	default:
		d.catchSetEnd(sess, tEnd, "")
	}
	var endAt uint64 // pipeline position of the first line at/after T_end
	pipe := NewPipeline(d.Store, boot, loc)
	pipe.NeedDropbox = d.requestDropbox
	pipe.DeviceNow = func() int64 { return d.Clock().Now.UnixNano() }
	pipe.OnUnknownUID = func(uid int) { d.maybeRefreshPackages(ctx, uid) }
	pipe.Clock = &d.lc
	pipe.OnClockEvent = d.onClockEvent
	// lines logged before the probe are history: their processes are
	// mostly gone, and psLoop seeds the table once caught up anyway
	liveFrom := devNow
	if liveFrom == 0 {
		liveFrom = tEnd
	}
	pipe.OnUnknownPID = func(newestNs int64) {
		if newestNs >= liveFrom {
			d.maybeSeedPS(ctx)
		}
	}
	d.mu.Lock()
	d.pipe = pipe
	d.mu.Unlock()
	// no side work (ps, pm, dumpsys, buffer span) while backfilling: on a
	// slow transport it competes with the stream (issue #11 follow-up)
	caught := d.caughtUp()
	d.bg.spawn("sideWork", func() { d.sideWork(ctx, caught, pipe, newBoot) })
	recovered := make(chan []*model.Record, 4)
	d.bg.spawn("reconcile", func() { d.reconcileLoop(ctx, caught, recovered) })

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Note: never rely on `logcat --uid` (missing on API 29) or multiple
	// `--pid` (only one is honoured); all filtering happens on our side.
	execOut := d.useExecOut()
	cmd := adbCmd(cctx, d.adb.Path, streamArgs(d.Addr(), resume, execOut)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer trackChild(cctx, cmd)() // unregistered after the Wait below
	d.mu.Lock()
	d.recording = true
	d.lastErr = ""
	d.mu.Unlock()
	if !sameBoot {
		d.bg.spawn("measureTotal", func() { d.measureTotal(ctx) }) // whole buffer: estimate its line count
	}
	if resume != "" {
		d.Store.UpdateMeta(func(m *store.Meta) {
			open := false
			for _, g := range m.Gaps {
				open = open || g.ToNs <= 0
			}
			if !open && m.LastSeenHostNs != 0 && time.Since(time.Unix(0, m.LastSeenHostNs)) > 5*time.Second {
				m.Gaps = append(m.Gaps, store.Gap{FromNs: m.ArrivalNs(), ToNs: -1, Reason: IdleGapReason})
			}
		})
	}

	lines := make(chan string, 20000)
	var gotLine atomic.Bool
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			gotLine.Store(true)
			lines <- strings.TrimRight(sc.Text(), "\r") // CRLF: `adb logcat` through a pty (old devices)
		}
		close(lines)
	}()
	waited := false
	defer func() {
		if waited {
			return
		}
		// stopped (idle, disconnect, disk): kill logcat and reap it, so no
		// zombie is left behind; let the reader goroutine finish
		cancel()
		go func() {
			for range lines {
			}
		}()
		cmd.Wait()
	}()

	asm := &logcat.Assembler{Loc: loc, Year: time.Now().Year()}
	var lastLine time.Time
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	flushT := time.NewTicker(500 * time.Millisecond)
	defer flushT.Stop()
	closeGapPending := true
	overlap := false // device buffer still held lines we already stored → nothing lost

	pipe.BeforeAppend = func(first *model.Record) {
		if !closeGapPending {
			return
		}
		closeGapPending = false
		d.Store.UpdateMeta(func(m *store.Meta) {
			kept := m.Gaps[:0]
			for _, g := range m.Gaps {
				if g.ToNs <= 0 {
					if overlap && g.ToNs < 0 {
						// resumed with -T and the boundary line was still in the
						// device buffer: no loss, so no gap to report
						continue
					}
					g.ToNs = first.TimeNs
				}
				kept = append(kept, g)
			}
			m.Gaps = kept
		})
	}
	off0, off0ok := d.clockOffset() // the connect clock read
	if tEnd == 0 && terr == nil {
		pipe.Live = true // empty buffer: every line is live
	}
	agree := 0
	var prevAt time.Time
	seenEnd := 0 // streamed records with the T_end key
	accept := func(r *model.Record) {
		if r == nil {
			return
		}
		now := time.Now()
		quiet := !prevAt.IsZero() && now.Sub(prevAt) >= time.Second
		prevAt = now
		atEnd := false
		if tEnd != 0 && !pipe.Live {
			if keyOf(r) == end.k {
				seenEnd++
				atEnd = seenEnd >= end.n
			} else {
				atEnd = replayEndLate(r, tEnd, now, off0, off0ok, quiet)
			}
		}
		if atEnd {
			// the line at T_end is the last replayed one: lines after it
			// arrive as they are logged, so their arrival host time can
			// set the clock offset (issue #48)
			defer func() { pipe.Live = true }()
		} else if tEnd == 0 && terr != nil && !pipe.Live && off0ok {
			// T_end unknown: live once two consecutive lines agree with
			// the connect clock read
			if lineAgrees(r, time.Now(), off0) {
				agree++
				if agree >= 2 {
					defer func() { pipe.Live = true }()
				}
			} else {
				agree = 0
			}
		}
		// de-duplication against stored lines applies to the replayed
		// part only: once live, a line dated before the last stored one
		// is new (the device clock was set back; issue #48)
		replaying := !pipe.Live
		if replaying && known != nil && known.take(recHash(r)) {
			overlap = true
			if whole {
				d.rollbackSkipped.Add(1)
			}
			if atEnd {
				d.catchDone(sess)
			}
			return
		}
		pipe.Accept(r, time.Now())
		if atEnd && endAt == 0 {
			endAt = pipe.Accepted()
		}
	}
	drain := func(all bool) {
		if err := pipe.Drain(all, time.Now()); err != nil {
			d.setErr(err)
		}
		if endAt != 0 && pipe.Appended() >= endAt {
			endAt = 0
			d.catchDone(sess) // the line at T_end is stored
		}
	}
	for {
		select {
		case ln, ok := <-lines:
			if !ok {
				accept(asm.Flush())
				drain(true)
				d.Store.Flush()
				werr := cmd.Wait()
				waited = true
				if ctx.Err() != nil {
					return nil
				}
				msg := strings.TrimSpace(stderr.String())
				if execOut && !gotLine.Load() && d.execOutFailed(werr, msg) {
					return errExecOut
				}
				if execOut && gotLine.Load() {
					d.execOut.Store(1)
				}
				d.Store.UpdateMeta(func(m *store.Meta) {
					m.Gaps = append(m.Gaps, store.Gap{FromNs: m.ArrivalNs(), ToNs: -1, Reason: "logcat stream ended (device disconnected?); resumed later with logcat -T, so only lines the device buffer dropped meanwhile are lost"})
				})
				return fmt.Errorf("logcat exited: %v %s", werr, firstLines(msg, 3))
			}
			lastLine = time.Now()
			if execOut && d.execOut.Load() == 0 {
				d.execOut.Store(1)
			}
			accept(asm.Push(ln))
		case rs := <-recovered:
			d.storeRecovered(pipe, rs)
		case <-tick.C:
			if asm.HasPending() && time.Since(lastLine) > 150*time.Millisecond {
				accept(asm.Flush())
			}
			drain(false)
			pipe.Tick(0)
		case <-flushT.C:
			d.Store.Flush()
			if d.disk.Paused() {
				accept(asm.Flush())
				drain(true)
				d.Store.Flush()
				recordPause(d.Store)
				return errDiskPaused
			}
		case <-ctx.Done():
			drain(true)
			d.Store.Flush()
			return nil
		}
	}
}

var epochLine = regexp.MustCompile(`^\s*(\d{9,})\.(\d+)\s`)

// measureSpan measures how much time the device's main log buffer holds
// (now - its oldest line): the service lingers about twice that long after
// its last client (issue #3). Read-only: the device-side `head` stops the
// dump after the first lines.
func (d *Device) measureSpan(ctx context.Context) {
	out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, "logcat -d -b main -v epoch | head -n 3")
	if err != nil {
		return
	}
	for _, ln := range strings.Split(out, "\n") {
		m := epochLine.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		if span := d.Now().Sub(time.Unix(sec, 0)); span > 0 {
			d.spanNs.Store(int64(span))
		}
		return
	}
}

// ReplaySpan is the last measured span of the main log buffer (0: unknown).
func (d *Device) ReplaySpan() time.Duration { return time.Duration(d.spanNs.Load()) }

func (d *Device) maybeSeedPS(ctx context.Context) {
	if !d.sideWorkOn() {
		return // psLoop seeds the table once caught up
	}
	d.mu.Lock()
	if time.Since(d.unknownPID) < 5*time.Second {
		d.mu.Unlock()
		return
	}
	d.unknownPID = time.Now()
	pipe := d.pipe
	d.mu.Unlock()
	if pipe != nil {
		d.bg.spawn("seedPS", func() { d.seedPS(ctx, pipe) })
	}
}

// psLoop seeds the process table now and re-reconciles it every 30 s: a
// full ps snapshot also closes processes that died without a lifecycle line.
func (d *Device) psLoop(ctx context.Context, pipe *Pipeline) {
	for ctx.Err() == nil && d.pipeline() == pipe {
		if d.Store.IsWriter() { // writer heartbeat, also when the device is quiet
			d.Store.UpdateMeta(func(m *store.Meta) { m.LastSeenHostNs = time.Now().UnixNano() })
		}
		d.saveClock(false)
		d.seedPS(ctx, pipe)
		sleep(ctx, 30*time.Second)
	}
}

func (d *Device) seedPS(ctx context.Context, pipe *Pipeline) {
	out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, "ps -A -o PID,UID,NAME 2>/dev/null || ps")
	if err != nil {
		return
	}
	pipe.SeedPS(out, d.Now().UnixNano())
}

func (d *Device) refreshPackages(ctx context.Context) {
	d.mu.Lock()
	d.pkgRefreshAt = time.Now()
	d.mu.Unlock()
	for attempt := 0; attempt < 30 && ctx.Err() == nil; attempt++ {
		if d.readPackages(ctx) {
			return
		}
		sleep(ctx, 10*time.Second) // e.g. package manager not up yet during boot
	}
}

// readPackages reads `pm list packages -U` once and installs it.
func (d *Device) readPackages(ctx context.Context) bool {
	out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, "pm list packages -U 2>/dev/null || cmd package list packages -U")
	if err != nil {
		return false
	}
	m := procs.ParsePackagesU(out)
	if len(m) == 0 {
		return false
	}
	if p := d.pipeline(); p != nil {
		p.Procs.SetPackages(m)
	}
	WritePackages(d.Store, m)
	return true
}

// packagesRefreshEvery: the package list is re-read for a device process
// whose application uid it does not contain (an app installed after it was
// read) at most this often (issue #41).
var packagesRefreshEvery = newDur(60 * time.Second)

// maybeRefreshPackages re-reads the package list for an unknown application
// uid, at most once per packagesRefreshEvery, as the session's side work
// (an idle service process runs no adb).
func (d *Device) maybeRefreshPackages(ctx context.Context, uid int) {
	if !d.sideWorkOn() {
		return // sideWork reads the list once caught up
	}
	d.mu.Lock()
	if !d.pkgRefreshAt.IsZero() && time.Since(d.pkgRefreshAt) < packagesRefreshEvery.Get() {
		d.mu.Unlock()
		return
	}
	d.pkgRefreshAt = time.Now()
	d.mu.Unlock()
	log.Printf("%s: uid %d is not in the package list; reading it again", d.Serial, uid)
	d.bg.spawn("refreshPackages (unknown uid)", func() { d.readPackages(ctx) })
}

// Packages returns uid->packages (from this process or the shared cache file).
func (d *Device) Packages() map[int][]string { return LoadPackages(d.Store) }

// Crashes loads all crashes (last version of each ID wins), newest first.
// Crashes returns crash summaries (newest version of each, newest first;
// no stack, traces or DropBox text). Shared: do not modify. Use CrashFull
// for the full crash.
func (d *Device) Crashes() []*analysis.Crash { return CrashSummaries(d.Store) }

// CrashFull decodes the newest version of the given crashes in full (nil
// for ids that are not stored).
func (d *Device) CrashFull(ids ...string) []*analysis.Crash { return CrashFull(d.Store, ids...) }

// Procs loads process lifetimes (last version of each entry wins).
func (d *Device) Procs() []procs.Proc { return LoadProcs(d.Store) }

// DropBox fetch states of a tag (Device.dbxPending).
const (
	dbxQueued  = 1 // a fetch will start after dropboxDelay
	dbxRunning = 2 // a fetch is running
	dbxAgain   = 3 // running, and a crash after it started wants another
)

// dropboxDelay lets AMS write a crash's DropBox entry before it is read.
var dropboxDelay = newDur(4 * time.Second)

// dropboxRecent: after catch-up, a tag fetched successfully this recently
// is not fetched again for crashes found in the backfill.
var dropboxRecent = newDur(5 * time.Minute)

// requestDropbox asks for a DropBox fetch of tag (a crash line wants its
// evidence). Before the session's side work starts (backfill) the tag is
// only noted; it is fetched once after catch-up (sideWork). A tag stays
// pending until its fetch completed: requests meanwhile share it, or —
// while it runs — queue exactly one more fetch.
func (d *Device) requestDropbox(tag string) {
	tags := []string{tag}
	if tag == "data_app_native_crash" {
		tags = append(tags, "SYSTEM_TOMBSTONE") // full tombstone, all threads
	}
	d.dbxMu.Lock()
	defer d.dbxMu.Unlock()
	for _, tag := range tags {
		if !d.sideOn {
			d.dbxDefer[tag] = true
			continue
		}
		switch d.dbxPending[tag] {
		case dbxQueued, dbxAgain:
		case dbxRunning:
			d.dbxPending[tag] = dbxAgain
		default:
			d.dbxPending[tag] = dbxQueued
			ctx := d.sideCtx
			d.bg.spawn("dropboxWorker "+tag, func() { d.dropboxWorker(ctx, tag) })
		}
	}
}

// dropboxWorker fetches a queued tag after dropboxDelay, again while
// crashes arrived during a fetch.
// It runs in the context of the session that queued it: a stop cancels
// it, and the tag is fetched by the next session instead.
func (d *Device) dropboxWorker(ctx context.Context, tag string) {
	for {
		sleep(ctx, dropboxDelay.Get())
		if ctx.Err() != nil {
			d.dbxMu.Lock()
			delete(d.dbxPending, tag)
			d.dbxDefer[tag] = true
			d.dbxMu.Unlock()
			return
		}
		d.dbxMu.Lock()
		d.dbxPending[tag] = dbxRunning
		d.dbxMu.Unlock()
		d.fetchDropbox(ctx, tag)
		if ctx.Err() != nil {
			d.dbxMu.Lock()
			delete(d.dbxPending, tag)
			d.dbxDefer[tag] = true
			d.dbxMu.Unlock()
			return
		}
		if !d.dropboxFinished(tag) {
			return
		}
	}
}

// dropboxFinished ends a fetch of tag; true: another one is wanted (queued).
func (d *Device) dropboxFinished(tag string) bool {
	d.dbxMu.Lock()
	defer d.dbxMu.Unlock()
	if d.dbxPending[tag] == dbxAgain {
		d.dbxPending[tag] = dbxQueued
		return true
	}
	delete(d.dbxPending, tag)
	return false
}

// dropboxHistory are the tags read on a new boot (crashes from before
// recording started).
var dropboxHistory = []string{"data_app_crash", "data_app_native_crash", "SYSTEM_TOMBSTONE", "data_app_anr"}

func (d *Device) setSideWork(on bool) {
	d.dbxMu.Lock()
	d.sideOn = on
	d.dbxMu.Unlock()
}

func (d *Device) sideWorkOn() bool {
	d.dbxMu.Lock()
	defer d.dbxMu.Unlock()
	return d.sideOn
}

// sideWorkWait: side work starts at the latest this long after recording
// started, even when the backfill has not caught up by then.
var sideWorkWait = newDur(time.Minute)

// sideWork starts the session's other adb work once caught up (or after
// sideWorkWait): the process table (ps), packages, the buffer span, and
// one DropBox fetch per tag that crashes in the backfill asked for,
// merged with the new-boot history tags.
func (d *Device) sideWork(ctx context.Context, caught <-chan struct{}, pipe *Pipeline, newBoot bool) {
	t := time.NewTimer(sideWorkWait.Get())
	defer t.Stop()
	select {
	case <-caught:
	case <-t.C:
		log.Printf("%s: not caught up after %s; starting ps/pm/dumpsys anyway", d.Serial, sideWorkWait.Get())
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil || d.pipeline() != pipe {
		return
	}
	d.dbxMu.Lock()
	d.sideOn = true
	d.sideCtx = ctx
	var tags []string
	if newBoot {
		tags = append(tags, dropboxHistory...)
	}
	var deferred []string
	for t := range d.dbxDefer {
		if !slices.Contains(tags, t) {
			deferred = append(deferred, t)
		}
	}
	slices.Sort(deferred)
	tags = append(tags, deferred...)
	clear(d.dbxDefer)
	d.dbxMu.Unlock()
	d.bg.spawn("measureSpan", func() { d.measureSpan(ctx) })
	d.bg.spawn("refreshPackages", func() { d.refreshPackages(ctx) })
	d.bg.spawn("psLoop", func() { d.psLoop(ctx, pipe) })
	for _, tag := range tags {
		if ctx.Err() != nil {
			d.deferDropbox(tag)
			continue // stopped meanwhile: the next session fetches it
		}
		if when, ok := d.dropboxFetchedAt(tag); ok && time.Since(when) < dropboxRecent.Get() {
			continue // read recently: already holds these crashes
		}
		d.dbxMu.Lock()
		if d.dbxPending[tag] != 0 {
			d.dbxMu.Unlock()
			continue // a live crash's fetch is coming anyway
		}
		d.dbxPending[tag] = dbxRunning
		d.dbxMu.Unlock()
		d.fetchDropbox(ctx, tag)
		if ctx.Err() != nil {
			d.dbxMu.Lock()
			delete(d.dbxPending, tag)
			d.dbxDefer[tag] = true
			d.dbxMu.Unlock()
			continue
		}
		if d.dropboxFinished(tag) {
			d.bg.spawn("dropboxWorker "+tag, func() { d.dropboxWorker(ctx, tag) })
		}
	}
}

func (d *Device) deferDropbox(tag string) {
	d.dbxMu.Lock()
	d.dbxDefer[tag] = true
	d.dbxMu.Unlock()
}

// dropboxFetchedAt is when tag was last fetched successfully.
func (d *Device) dropboxFetchedAt(tag string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(d.Store.Dir, "dropbox_status_"+tag))
	if err != nil {
		return time.Time{}, false
	}
	ts, status, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	t, err := time.Parse(time.RFC3339, ts)
	return t, err == nil && status == "ok"
}

// fetchDropbox runs `dumpsys dropbox --print <tag>` and merges entries.
func (d *Device) fetchDropbox(ctx context.Context, tag string) {
	out, err := d.adb.Shell(ctx, d.Addr(), 30*time.Second, "dumpsys dropbox --print "+tag)
	if ctx.Err() != nil {
		return // stopped: no status to report
	}
	status := "ok"
	if err != nil {
		status = err.Error()
	} else if strings.Contains(out, "Permission Denial") {
		status = "permission denied"
	}
	os.WriteFile(filepath.Join(d.Store.Dir, "dropbox_status_"+tag), []byte(time.Now().Format(time.RFC3339)+" "+status), 0o644)
	if status != "ok" {
		return
	}
	pipe := d.pipeline()
	if pipe == nil {
		return
	}
	pipe.ApplyDropbox(out)
}

// DropboxStatus reports the last dropbox fetch result per tag.
func (d *Device) DropboxStatus(tag string) string {
	b, err := os.ReadFile(filepath.Join(d.Store.Dir, "dropbox_status_"+tag))
	if err != nil {
		return "not fetched yet"
	}
	return string(b)
}

// parseDate parses `date +%s.%N` (or `date +%s`, or toybox printing a
// literal N): the time and whether it has only whole-second precision.
func parseDate(s string) (time.Time, bool, bool) {
	s = strings.TrimSpace(s)
	secS, frac, hasFrac := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secS, 10, 64)
	if err != nil || sec <= 0 { // a device clock reset to 1970 is a valid reading (#48)
		return time.Time{}, false, false
	}
	if hasFrac && frac != "" && strings.Trim(frac, "0123456789") == "" {
		ns, _ := strconv.ParseInt((frac + "000000000")[:9], 10, 64)
		return time.Unix(sec, ns), false, true
	}
	return time.Unix(sec, 0), true, true
}

// started reports whether this process records the device and its
// recorder session runs (setState started it; catch-up began then).
func (d *Device) started() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.owned && d.cancel != nil
}

// The replay ends at the T_end record, the last buffered one by arrival
// when the stream started (#30 catch-up): matched by its header (endMark),
// not by "device time ≥ T_end", since after a clock jump replayed lines
// can be dated after T_end (issues #48, #52 D3). replayEndLate is the
// fallback when that record never arrives (evicted): after the stream was
// quiet for a while (the replay burst is over), a line at or after T_end
// that agrees with the connect clock read ends the replay.
func replayEndLate(r *model.Record, tEnd int64, arrived time.Time, off0 int64, ok, quiet bool) bool {
	return quiet && r.TimeNs >= tEnd && ok && lineAgrees(r, arrived, off0)
}

// lineAgrees: r's device time against its arrival host time is within the
// jump threshold of the clock offset off.
func lineAgrees(r *model.Record, arrived time.Time, off int64) bool {
	d := r.TimeNs - arrived.UnixNano() - off
	return d <= store.ClockJumpThreshold && d >= -store.ClockJumpThreshold
}
