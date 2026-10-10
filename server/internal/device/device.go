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

	mu         sync.RWMutex
	state      string // adb state ("" = not attached)
	recording  bool
	lastErr    string
	loc        *time.Location
	tzName     string
	offset     time.Duration // device clock - host clock
	offsetAt   time.Time     // when offset was measured (host time)
	bootID     string
	pipe       *Pipeline // current boot session (writer only)
	cancel     context.CancelFunc
	lastSeen   time.Time
	unknownPID time.Time // debounce for ps refresh
	pkgRefresh time.Time
	dbxPending map[string]bool
	dbxMu      sync.Mutex
	crashMu    sync.Mutex // serializes ID assignment + persistence
	disk       *store.DiskGuard
	runWG      sync.WaitGroup // the recorder goroutine (run)
	spanNs     atomic.Int64   // replay span of the device's main log buffer (measureSpan)

	catch           catchState   // backfill / catch-up of the current recording session
	devNowNs        int64        // device clock read by the last probe (0 = unknown)
	devNowCoarse    bool         // devNowNs has whole-second precision only
	rollback        string       // last clock rollback (RollbackNote)
	rollbackSkipped atomic.Int64 // lines dropped as already stored after it
}

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
		m.Gaps = append(m.Gaps, store.Gap{FromNs: m.LastTimeNs, ToNs: -1, Reason: PauseGapReason})
	})
}

func newDevice(adb *ADB, st *store.Store, serial string) *Device {
	d := &Device{Serial: serial, Store: st, adb: adb, dbxPending: map[string]bool{}}
	m := st.Meta()
	d.addr = m.Addr
	if d.addr == "" {
		d.addr = serial
	}
	d.applyTZ(m.TZ, m.TZOffset)
	d.offset = time.Duration(m.ClockOffsetNs)
	d.offsetAt = time.Unix(0, m.ClockHostNs)
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

// Now estimates the device clock.
// The offset is re-measured every 30 s by the recorder (emulators under
// software emulation drift by minutes); followers use the recorder's newer
// measurement from meta.json.
func (d *Device) Now() time.Time {
	d.mu.RLock()
	off, at, loc := d.offset, d.offsetAt, d.loc
	d.mu.RUnlock()
	if d.Store != nil {
		if m := d.Store.Meta(); m.ClockHostNs > at.UnixNano() {
			off = time.Duration(m.ClockOffsetNs)
		}
	}
	return time.Now().Add(off).In(loc)
}

// measureClock re-reads the device clock (`date +%s.%N`, midpoint of the adb round trip).
func (d *Device) measureClock(ctx context.Context) {
	t0 := time.Now()
	out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, "date +%s.%N")
	t1 := time.Now()
	if err != nil {
		return
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil || f < 1e9 {
		return
	}
	sec := int64(f)
	dev := time.Unix(sec, int64((f-float64(sec))*1e9))
	mid := t0.Add(t1.Sub(t0) / 2)
	off := dev.Sub(mid)
	d.mu.Lock()
	d.offset, d.offsetAt = off, mid
	d.mu.Unlock()
	d.Store.UpdateMeta(func(m *store.Meta) {
		m.ClockOffsetNs, m.ClockHostNs = int64(off), mid.UnixNano()
		m.LastSeenHostNs = time.Now().UnixNano() // writer heartbeat (also when the device is quiet)
	})
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
		d.catchBegin() // a call right after attach waits for this session's catch-up
		cctx, cancel := context.WithCancel(ctx)
		d.mu.Lock()
		d.cancel = cancel
		d.runWG.Add(1)
		d.mu.Unlock()
		go func() {
			defer d.runWG.Done()
			defer d.catchDone() // nothing is backfilling any more
			d.run(cctx)
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
	d.catchDone()
}

// probe reads device properties, boot id and clock offset (read-only).
func (d *Device) probe(ctx context.Context) error {
	cmd := "getprop ro.product.model; getprop ro.build.version.release; getprop ro.build.version.sdk; getprop persist.sys.timezone; date +%z; cat /proc/sys/kernel/random/boot_id; date +%s.%N"
	t0 := time.Now()
	out, err := d.adb.Shell(ctx, d.Addr(), 15*time.Second, cmd)
	t1 := time.Now()
	if err != nil {
		return err
	}
	l := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(l) < 7 {
		l = append(l, "")
	}
	for i := range l {
		l[i] = strings.TrimSpace(l[i])
	}
	d.applyTZ(l[3], l[4])
	if now, coarse, ok := parseDate(l[6]); ok {
		mid := t0.Add(t1.Sub(t0) / 2)
		est := now
		if coarse {
			// date +%s truncates: add 0.5 s to center the estimate.
			est = now.Add(500 * time.Millisecond)
		}
		off := est.Sub(mid)
		d.mu.Lock()
		d.offset, d.offsetAt = off, mid
		d.devNowNs, d.devNowCoarse = now.UnixNano(), coarse
		d.mu.Unlock()
	} else {
		d.mu.Lock()
		d.devNowNs = 0
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.bootID = l[5]
	off := d.offset
	d.mu.Unlock()
	d.Store.UpdateMeta(func(m *store.Meta) {
		m.Model, m.Release, m.SDK, m.TZ, m.TZOffset, m.ClockOffsetNs = l[0], l[1], l[2], l[3], l[4], int64(off)
		m.ClockHostNs = time.Now().UnixNano()
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
		if errors.Is(err, errDiskPaused) {
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
	d.catchBegin()

	resume := ""
	var dedupNs int64
	dedup := map[uint64]bool{}
	var rbSet map[uint64]bool // clock rollback: stored lines of this boot still in the buffer
	sameBoot := meta.LastBootID == bootID && meta.LastTimeNs > 0
	if sameBoot && devNow != 0 {
		limit := devNow
		if coarse {
			limit += int64(time.Second)
		}
		if meta.LastTimeNs > limit {
			// -T <last stored time> would name a future time and print
			// nothing until then: read the whole buffer instead and drop
			// what is already stored (issue #11 §4)
			d.noteRollback(meta.LastTimeNs, devNow, loc)
			rbSet = rollbackSet(d.Store, d.measureStart(ctx))
		}
	}
	if rbSet != nil {
		// same boot session: nothing to set up
	} else if sameBoot {
		resume = logcat.FormatResumeTime(meta.LastTimeNs)
		dedupNs = meta.LastTimeNs
		// hashes of stored records at the boundary timestamp
		last := d.Store.LastSeq()
		d.Store.Scan(0, last+1, true, func(r *model.Record) bool {
			if r.TimeNs < dedupNs {
				return false
			}
			dedup[recHash(r)] = true
			return true
		})
		if rd := meta.ResumeDedup; rd != nil && rd.TimeNs == dedupNs {
			for _, h := range rd.Hashes { // stored in the previous root (set-root)
				dedup[h] = true
			}
		}
	} else {
		// new boot (or first contact): import the device's whole ring buffer
		now := time.Now().UnixNano()
		nextSeq := d.Store.LastSeq() + 1 // not inside UpdateMeta: store lock is held there
		d.Store.UpdateMeta(func(m *store.Meta) {
			if m.LastBootID != "" && m.LastBootID != bootID {
				m.Gaps = append(m.Gaps, store.Gap{FromNs: m.LastTimeNs, ToNs: 0, Reason: "device rebooted (logs from before the reboot that were not yet recorded are lost)"})
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
	go d.measureSpan(ctx)
	// T_end: the newest buffered line now; caught up once a streamed line
	// at or after it has been stored or deduplicated
	tEnd, terr := d.measureEnd(ctx)
	if terr != nil && ctx.Err() == nil {
		tEnd, terr = d.measureEnd(ctx)
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case terr != nil:
		log.Printf("%s: measuring the end of the device buffer failed (%v); catch-up is not verified", d.Serial, terr)
		d.catchSetEnd(0, "could not measure the end of the device buffer: "+terr.Error())
		d.catchDone()
	case tEnd == 0:
		d.catchDone() // empty buffer
	case resume != "" && tEnd <= dedupNs:
		d.catchSetEnd(tEnd, "")
		d.catchDone() // nothing newer than what is stored
	default:
		d.catchSetEnd(tEnd, "")
	}
	var endAt uint64 // pipeline position of the first line at/after T_end
	pipe := NewPipeline(d.Store, boot, loc)
	pipe.NeedDropbox = d.requestDropbox
	pipe.OnUnknownPID = func() { d.maybeSeedPS(ctx) }
	d.mu.Lock()
	d.pipe = pipe
	d.mu.Unlock()
	go d.refreshPackages(ctx)
	go d.psLoop(ctx, pipe)
	if meta.LastBootID != bootID {
		go func() { // dropbox history (crashes from before recording)
			sleep(ctx, 3*time.Second)
			for _, t := range []string{"data_app_crash", "data_app_native_crash", "SYSTEM_TOMBSTONE", "data_app_anr"} {
				if ctx.Err() != nil {
					return // stopped meanwhile
				}
				d.fetchDropbox(ctx, t)
			}
		}()
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Note: never rely on `logcat --uid` (missing on API 29) or multiple
	// `--pid` (only one is honoured); all filtering happens on our side.
	cmd := adbCmd(cctx, d.adb.Path, append([]string{"-s", d.Addr()}, logcat.Args(resume)...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.recording = true
	d.lastErr = ""
	d.mu.Unlock()
	if resume != "" {
		d.Store.UpdateMeta(func(m *store.Meta) {
			open := false
			for _, g := range m.Gaps {
				open = open || g.ToNs <= 0
			}
			if !open && m.LastSeenHostNs != 0 && time.Since(time.Unix(0, m.LastSeenHostNs)) > 5*time.Second {
				m.Gaps = append(m.Gaps, store.Gap{FromNs: m.LastTimeNs, ToNs: -1, Reason: IdleGapReason})
			}
		})
	}

	lines := make(chan string, 20000)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
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
	accept := func(r *model.Record) {
		if r == nil {
			return
		}
		atEnd := tEnd != 0 && r.TimeNs >= tEnd
		if rbSet != nil {
			// after a clock rollback times are not monotonic: the line at
			// T_end is the one with that time (T_end has ms precision)
			atEnd = tEnd != 0 && r.TimeNs/1e6 == tEnd/1e6
		}
		if dedupNs != 0 && r.TimeNs <= dedupNs {
			overlap = true
			if r.TimeNs < dedupNs || dedup[recHash(r)] {
				if atEnd {
					d.catchDone()
				}
				return
			}
		}
		if rbSet != nil && rbSet[recHash(r)] {
			overlap = true
			d.rollbackSkipped.Add(1)
			if atEnd {
				d.catchDone()
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
			d.catchDone() // the line at T_end is stored
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
				d.Store.UpdateMeta(func(m *store.Meta) {
					m.Gaps = append(m.Gaps, store.Gap{FromNs: m.LastTimeNs, ToNs: -1, Reason: "logcat stream ended (device disconnected?); resumed later with logcat -T, so only lines the device buffer dropped meanwhile are lost"})
				})
				return fmt.Errorf("logcat exited: %v %s", werr, firstLines(msg, 3))
			}
			lastLine = time.Now()
			accept(asm.Push(ln))
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
	d.mu.Lock()
	if time.Since(d.unknownPID) < 5*time.Second {
		d.mu.Unlock()
		return
	}
	d.unknownPID = time.Now()
	pipe := d.pipe
	d.mu.Unlock()
	if pipe != nil {
		go d.seedPS(ctx, pipe)
	}
}

// psLoop seeds the process table now and re-reconciles it every 30 s: a
// full ps snapshot also closes processes that died without a lifecycle line.
func (d *Device) psLoop(ctx context.Context, pipe *Pipeline) {
	for ctx.Err() == nil && d.pipeline() == pipe {
		d.measureClock(ctx)
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
	for attempt := 0; attempt < 30 && ctx.Err() == nil; attempt++ {
		out, err := d.adb.Shell(ctx, d.Addr(), 20*time.Second, "pm list packages -U 2>/dev/null || cmd package list packages -U")
		if err == nil {
			if m := procs.ParsePackagesU(out); len(m) > 0 {
				if p := d.pipeline(); p != nil {
					p.Procs.SetPackages(m)
				}
				WritePackages(d.Store, m)
				return
			}
		}
		sleep(ctx, 10*time.Second) // e.g. package manager not up yet during boot
	}
}

// Packages returns uid->packages (from this process or the shared cache file).
func (d *Device) Packages() map[int][]string { return LoadPackages(d.Store) }

// Crashes loads all crashes (last version of each ID wins), newest first.
func (d *Device) Crashes() []*analysis.Crash { return LoadCrashes(d.Store) }

// Procs loads process lifetimes (last version of each entry wins).
func (d *Device) Procs() []procs.Proc { return LoadProcs(d.Store) }

func (d *Device) requestDropbox(tag string) {
	tags := []string{tag}
	if tag == "data_app_native_crash" {
		tags = append(tags, "SYSTEM_TOMBSTONE") // full tombstone, all threads
	}
	for _, tag := range tags {
		d.dbxMu.Lock()
		if d.dbxPending[tag] {
			d.dbxMu.Unlock()
			continue
		}
		d.dbxPending[tag] = true
		d.dbxMu.Unlock()
		go func(tag string) {
			time.Sleep(4 * time.Second) // let AMS write the entry
			d.dbxMu.Lock()
			delete(d.dbxPending, tag)
			d.dbxMu.Unlock()
			d.fetchDropbox(context.Background(), tag)
		}(tag)
	}
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
	if err != nil || sec < 1e8 {
		return time.Time{}, false, false
	}
	if hasFrac && frac != "" && strings.Trim(frac, "0123456789") == "" {
		ns, _ := strconv.ParseInt((frac + "000000000")[:9], 10, 64)
		return time.Unix(sec, ns), false, true
	}
	return time.Unix(sec, 0), true, true
}
