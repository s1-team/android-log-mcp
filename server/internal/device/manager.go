package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Manager tracks adb devices and their stores. Devices are keyed by their
// stable ID (ResolveID); the adb serial is only the connection address.
type Manager struct {
	// Base is the data root: layout.DefaultDir()
	// (os.UserCacheDir()/android-log-mcp; fixed since #10), or the
	// --offline dataset.
	Base string
	// Root is the data root (Dir = Base) plus the owners directory, which is
	// fixed per user (layout.DefaultOwnersDir), so a device is recorded by
	// one process per user.
	Root       layout.Root
	Version    string
	Cfg        store.Config
	MaxDevices int
	// NoADB (--offline <dir>): browse a dataset read-only; adb is not used
	// and nothing is recorded, pruned or created.
	NoADB bool
	// MaxTotalBytes caps everything the tool manages under Base (all
	// devices and format versions, tmp/, run/); 0 disables. Default 2 GiB.
	MaxTotalBytes int64
	// Disk pauses recording while free space on the data disk is low.
	Disk *store.DiskGuard

	adb       *ADB
	adbErr    error
	ctx       context.Context
	mu        sync.Mutex
	devices   map[string]*Device // by device ID
	states    map[string]string  // live adb states by adb serial (incl. unauthorized/offline)
	addrs     map[string]string  // adb serial -> device ID
	resolving map[string]bool
	attachMu  sync.Mutex // serializes attach / migration / sweep
	watchErr  error
	closed    bool // Close was called: never attach again
	ready     chan struct{}
	once      sync.Once
}

func NewManager(base string, cfg store.Config) *Manager {
	m := &Manager{Base: base, Root: layout.Root{Dir: base, OwnersDir: layout.DefaultOwnersDir()}, Cfg: cfg, MaxDevices: 4,
		devices: map[string]*Device{}, states: map[string]string{}, addrs: map[string]string{}, resolving: map[string]bool{},
		ready: make(chan struct{}), MaxTotalBytes: DefaultMaxTotalBytes, Disk: &store.DiskGuard{MinFree: DefaultMinFreeBytes}}
	if v, err := strconv.Atoi(os.Getenv("ANDROID_LOG_MCP_MAX_DEVICES")); err == nil && v > 0 {
		m.MaxDevices = v
	}
	if v, ok := store.EnvInt64("ANDROID_LOG_MCP_MAX_TOTAL_MB"); ok {
		m.MaxTotalBytes = v << 20 // 0 disables
	}
	if v, ok := store.EnvInt64("ANDROID_LOG_MCP_MIN_FREE_MB"); ok {
		m.Disk.MinFree = uint64(v) << 20 // 0 disables
	}
	return m
}

// Disk protection defaults.
const (
	DefaultMaxTotalBytes = 2 << 30
	DefaultMinFreeBytes  = 512 << 20 // resume at 2× this
)

func (m *Manager) newDevice(st *store.Store, serial string) *Device {
	d := newDevice(m.adb, st, serial)
	d.disk = m.Disk
	return d
}

func (m *Manager) maxAge() time.Duration {
	if m.Cfg.MaxAge > 0 {
		return m.Cfg.MaxAge
	}
	return store.DefaultMaxAge
}

// maintain runs the disk protection: the free-space guard (every 10 s), the
// sweep of the data root (retention + size cap, every minute) and aux-file
// compaction of the devices this process records (shortly after start, then
// every 10 min). Every tick it also retries devices whose identity is not
// resolved yet and takes over devices whose foreign owner went away.
func (m *Manager) maintain(ctx context.Context) {
	m.Disk.Check(m.Base)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		if paused, changed := m.Disk.Check(m.Base); changed {
			if paused {
				log.Print(m.Disk.Reason())
			} else {
				log.Print("free disk space recovered; recording resumes")
			}
		}
		for _, d := range m.Devices() {
			d.owner.Refresh() // fallback lock platforms only (neither unix nor windows)
		}
		m.recheck()
		if n%6 == 0 {
			m.MaintainOnce(false)
		}
		if n == 3 || n%60 == 0 {
			m.MaintainOnce(true)
		}
	}
}

// MaintainOnce applies retention (per device, then a sweep of everything
// under the data root: other devices, other format versions, tmp/, run/,
// the total size cap) and, with aux, compacts the auxiliary files of the
// devices this process writes.
func (m *Manager) MaintainOnce(aux bool) {
	if m.NoADB {
		return // --offline: never prune or delete anything
	}
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	if m.closed {
		return
	}
	owned := map[string]*store.Store{}
	for _, d := range m.Devices() {
		if d.Owned() && d.Store.IsWriter() {
			owned[d.Store.Dir] = d.Store
			d.Store.Prune() // age / size retention, also while no segment rotates
			if aux {
				CompactAux(d.Store, time.Now())
			}
		}
	}
	m.sweepLocked(owned)
	m.dropVanished()
}

func (m *Manager) sweepLocked(owned map[string]*store.Store) layout.SweepResult {
	res := layout.Sweep(layout.SweepOptions{Base: m.Base, Root: &m.Root, MaxTotal: m.MaxTotalBytes, MaxAge: m.maxAge(), Owned: owned})
	if res.CapTriggered && res.Freed > 0 {
		log.Printf("data directory over %d MiB: removed %d MiB of the oldest logs (ANDROID_LOG_MCP_MAX_TOTAL_MB)", m.MaxTotalBytes>>20, res.Freed>>20)
	} else if res.Freed > 0 {
		log.Printf("retention: removed %d expired files (%d KiB) under %s", res.Files, res.Freed>>10, m.Base)
	}
	for _, d := range res.RemovedDirs {
		log.Printf("retention: removed expired directory %s", d)
	}
	return res
}

// dropVanished forgets devices whose (unowned) directory was removed.
func (m *Manager) dropVanished() {
	m.mu.Lock()
	var gone []*Device
	for k, d := range m.devices {
		if d.Owned() || d.Status().Owner != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(d.Store.Dir, "meta.json")); os.IsNotExist(err) {
			delete(m.devices, k)
			gone = append(gone, d)
		}
	}
	m.mu.Unlock()
	for _, d := range gone {
		d.stop()
		d.Store.Close()
	}
}

// Start begins device tracking (non-blocking).
func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	if !m.NoADB {
		// startup sweep (--offline only browses a dataset, e.g. an unzipped
		// report: never write, prune or delete anything there)
		if err := m.Root.Ensure(); err != nil {
			log.Printf("data root %s: %v", m.Root.Dir, err)
		}
		m.attachMu.Lock()
		m.sweepLocked(nil)
		m.attachMu.Unlock()
	}
	m.loadDirs()
	if !m.NoADB {
		go m.maintain(ctx)
	}
	if m.NoADB {
		m.adbErr = fmt.Errorf("adb disabled")
		m.once.Do(func() { close(m.ready) })
		return
	}
	p, err := FindADB()
	if err != nil {
		m.adbErr = err
		m.once.Do(func() { close(m.ready) })
		return
	}
	m.adb = &ADB{Path: p}
	m.mu.Lock()
	for _, d := range m.devices {
		d.adb = m.adb
	}
	m.mu.Unlock()
	go func() {
		// start the adb server outside our process group first (it is
		// shared with other tools and must outlive us), then watch
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := m.adb.StartServer(sctx); err != nil && ctx.Err() == nil {
			log.Printf("adb start-server: %v", err)
		}
		cancel()
		m.adb.Watch(ctx, m.onDevices)
	}()
	go func() { // don't block the first tool call forever
		sleep(ctx, 5*time.Second)
		m.once.Do(func() { close(m.ready) })
	}()
}

// loadDirs opens every recorded device directory under Base read-only:
// previously recorded devices are queryable even when offline (a device
// becomes recorded when it connects and is not owned elsewhere).
func (m *Manager) loadDirs() {
	for _, dir := range store.ListDeviceDirs(m.Base) {
		st, err := store.OpenReadOnly(dir, m.Cfg)
		if err != nil || st.Serial == "" {
			continue
		}
		m.mu.Lock()
		key := st.Serial
		if _, ok := m.devices[key]; ok {
			rel, _ := filepath.Rel(m.Base, dir)
			key += "@" + filepath.ToSlash(rel)
		}
		m.devices[key] = m.newDevice(st, key)
		m.mu.Unlock()
	}
}

// WaitReady waits (bounded) for the first device list.
func (m *Manager) WaitReady(timeout time.Duration) {
	select {
	case <-m.ready:
	case <-time.After(timeout):
	}
}

func (m *Manager) onDevices(list []DevState, err error) {
	m.mu.Lock()
	m.watchErr = err
	if err != nil {
		m.mu.Unlock()
		return
	}
	m.states = map[string]string{}
	for _, ds := range list {
		m.states[ds.Serial] = ds.State
	}
	for a := range m.addrs {
		if _, ok := m.states[a]; !ok {
			delete(m.addrs, a) // gone: the address may be reused by another device (emulator ports)
		}
	}
	pending := m.startResolvesLocked()
	m.mu.Unlock()
	m.applyStates()
	if pending == 0 {
		m.once.Do(func() { close(m.ready) })
	}
}

// startResolvesLocked probes the identity of online adb serials not mapped
// to a device ID yet. Returns how many are pending.
func (m *Manager) startResolvesLocked() int {
	n := 0
	for a, st := range m.states {
		if st != "device" {
			continue
		}
		if _, ok := m.addrs[a]; ok {
			continue
		}
		n++
		if !m.resolving[a] {
			m.resolving[a] = true
			go m.resolve(a)
		}
	}
	return n
}

func (m *Manager) resolve(addr string) {
	id, err := ProbeIdentity(m.ctx, m.adb, addr)
	if err != nil {
		m.mu.Lock()
		delete(m.resolving, addr)
		m.mu.Unlock()
		log.Printf("%s: identity probe failed (retrying): %v", addr, err)
		return
	}
	final := m.attach(id)
	m.mu.Lock()
	delete(m.resolving, addr)
	if m.states[addr] != "" && final != "" {
		m.addrs[addr] = final
	}
	pending := m.startResolvesLocked()
	m.mu.Unlock()
	m.applyStates()
	if pending == 0 {
		m.once.Do(func() { close(m.ready) })
	}
}

// applyStates pushes the live adb state to every device (by ID).
func (m *Manager) applyStates() {
	m.mu.Lock()
	type upd struct {
		d     *Device
		addr  string
		state string
	}
	byID := map[string]upd{}
	addrs := make([]string, 0, len(m.addrs))
	for a := range m.addrs {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		id := m.addrs[a]
		cur, ok := byID[id]
		if ok && cur.state == "device" {
			continue // already reachable through another transport
		}
		byID[id] = upd{addr: a, state: m.states[a]}
	}
	var ups []upd
	for k, d := range m.devices {
		u, ok := byID[k]
		if !ok {
			ups = append(ups, upd{d: d})
			continue
		}
		u.d = d
		ups = append(ups, u)
	}
	m.mu.Unlock()
	for _, u := range ups {
		if u.addr != "" {
			u.d.setAddr(u.addr)
		}
		u.d.setState(m.ctx, u.state)
	}
}

// recheck retries unresolved devices and re-attaches devices whose foreign
// owner (another process, or a pre-v1 version) has gone away.
func (m *Manager) recheck() {
	m.mu.Lock()
	m.startResolvesLocked()
	var retry []string
	for a, id := range m.addrs {
		if m.states[a] != "device" || m.resolving[a] {
			continue
		}
		d := m.devices[id]
		if d == nil {
			retry = append(retry, a) // e.g. over MaxDevices before
			continue
		}
		d.mu.RLock()
		f := d.foreign
		d.mu.RUnlock()
		if f != nil && !m.foreignLive(id, f) {
			retry = append(retry, a)
		}
	}
	for _, a := range retry {
		m.resolving[a] = true
		go m.resolve(a)
	}
	m.mu.Unlock()
}

func (m *Manager) foreignLive(id string, f *layout.Owner) bool {
	return m.Root.OwnerHeld(id)
}

// Attach is attach for a device whose identity is already known (tests,
// tools that probed it themselves). Returns the ID the device is filed under.
func (m *Manager) Attach(id Identity) string { return m.attach(id) }

// collision reports whether another device that is connected right now
// already uses id.ID: same ID, different adb serial and a different boot id
// (the same device over two transports, e.g. USB + Wi-Fi, has one boot id).
// Checked against this process's devices and the global owner lock, so it
// also holds across processes. Returns the other device's adb serial.
func (m *Manager) collision(id Identity) (string, bool) {
	if id.BootID == "" {
		return "", false
	}
	m.mu.Lock()
	cur := m.devices[id.ID]
	online := func(a string) bool { return a != "" && m.states[a] == "device" }
	m.mu.Unlock()
	if cur != nil && cur.Owned() {
		cur.mu.RLock()
		a, boot := cur.addr, cur.bootID
		cur.mu.RUnlock()
		m.mu.Lock()
		on := online(a)
		m.mu.Unlock()
		if a != id.Addr && on && boot != "" && boot != id.BootID {
			return a, true
		}
	}
	if o, held := m.Root.ReadOwner(id.ID); held && o != nil && o.Addr != id.Addr && o.BootID != "" && o.BootID != id.BootID {
		m.mu.Lock()
		on := online(o.Addr)
		m.mu.Unlock()
		if on {
			return o.Addr, true
		}
	}
	return "", false
}

// attach decides who records device id: this process (taking the global
// owner lock and recording into <base>/v1/<id>) or another one (follow its
// data read-only and report it). Returns
// the ID the device is filed under ("" if it is not tracked).
func (m *Manager) attach(id Identity) string {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	if m.NoADB || m.closed || (m.ctx != nil && m.ctx.Err() != nil) {
		return "" // --offline never records, follows or creates anything
	}
	if other, ok := m.collision(id); ok {
		base := id.ID
		id.ID = base + "~" + store.SanitizeSerial(id.Addr)
		id.Source += "+addr"
		id.Collision = fmt.Sprintf("device ID %s is already used by another connected device (adb %s, different boot); this device is filed separately as %s", base, other, id.ID)
		log.Printf("%s: %s", id.Addr, id.Collision)
	}
	m.mu.Lock()
	cur := m.devices[id.ID]
	m.mu.Unlock()
	if cur != nil {
		cur.mu.RLock()
		f := cur.foreign
		cur.mu.RUnlock()
		if cur.Owned() || (f != nil && m.foreignLive(id.ID, f)) {
			cur.setAddr(id.Addr)
			return id.ID
		}
	}
	owned := 0
	for _, d := range m.Devices() {
		if d.Owned() {
			owned++
		}
	}
	if owned >= m.MaxDevices {
		log.Printf("%s (%s): not recorded, already recording %d devices (ANDROID_LOG_MCP_MAX_DEVICES)", id.ID, id.Addr, owned)
		return id.ID
	}
	dir := layout.DeviceDir(m.Base, id.ID)
	info := layout.Owner{PID: os.Getpid(), Version: m.Version, DataDir: dir, Addr: id.Addr, BootID: id.BootID, Since: time.Now().UTC()}
	lock, other, err := m.Root.AcquireOwner(id.ID, info)
	if errors.Is(err, layout.ErrOwned) {
		if other == nil {
			other = &layout.Owner{}
		}
		m.follow(id, other)
		return id.ID
	}
	if err != nil {
		log.Printf("%s: owner lock: %v", id.ID, err)
		return ""
	}
	st, err := store.OpenDir(dir, id.ID, m.Cfg, true)
	if err != nil {
		lock.Release()
		log.Printf("%s: open %s: %v", id.ID, dir, err)
		return ""
	}
	st.UpdateMeta(func(mt *store.Meta) {
		mt.Serial, mt.Addr, mt.IDSource = id.ID, id.Addr, id.Source
	})
	d := m.newDevice(st, id.ID)
	d.addr = id.Addr
	d.owner = lock
	d.adb = m.adb
	d.idNote = id.Collision
	if id.BootID != "" {
		d.bootID = id.BootID
	}
	m.swap(id.ID, d)
	return id.ID
}

// follow makes device id a read-only follower of its owner's data.
func (m *Manager) follow(id Identity, o *layout.Owner) {
	dir := o.DataDir
	if dir == "" || !exists(filepath.Join(dir, "meta.json")) {
		dir = layout.DeviceDir(m.Base, id.ID) // owner unknown / not written yet: our own copy (if any)
	}
	m.mu.Lock()
	cur := m.devices[id.ID]
	m.mu.Unlock()
	if cur != nil && cur.Store.Dir == dir && !cur.Owned() {
		cur.mu.Lock()
		cur.foreign, cur.idNote = o, id.Collision
		cur.mu.Unlock()
		cur.setAddr(id.Addr)
		return
	}
	var st *store.Store
	var err error
	if exists(filepath.Join(dir, "meta.json")) {
		st, err = store.OpenReadOnly(dir, m.Cfg)
	} else {
		st, err = store.OpenDir(dir, id.ID, m.Cfg, false)
	}
	if err != nil {
		log.Printf("%s: follow %s: %v", id.ID, dir, err)
		return
	}
	d := m.newDevice(st, id.ID)
	d.addr = id.Addr
	d.foreign = o
	d.idNote = id.Collision
	d.adb = m.adb
	m.swap(id.ID, d)
	log.Printf("%s (%s): recorded by another android-log-mcp process (%s); following read-only", id.ID, id.Addr, o)
}

// swap installs d under key, retiring the previous device with that key and
// any other entry reading the same directory.
func (m *Manager) swap(key string, d *Device) {
	m.mu.Lock()
	var old []*Device
	for k, e := range m.devices {
		if k == key || e.Store.Dir == d.Store.Dir {
			delete(m.devices, k)
			old = append(old, e)
		}
	}
	m.devices[key] = d
	m.mu.Unlock()
	for _, e := range old {
		e.stop()
		e.owner.Release()
		go func(st *store.Store) { // tools may still hold the old device briefly
			time.Sleep(time.Minute)
			st.Close()
		}(e.Store)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ADBError is non-nil when adb could not be found.
func (m *Manager) ADBError() error { return m.adbErr }

// WatchError is the last device-listing error (adb server problems).
func (m *Manager) WatchError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watchErr
}

// ADB returns the adb wrapper (nil if not found).
func (m *Manager) ADB() *ADB { return m.adb }

// LiveStates returns adb's current serial->state (incl. unauthorized).
func (m *Manager) LiveStates() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.states {
		out[k] = v
	}
	return out
}

// Devices returns all devices with stores, sorted by serial.
func (m *Manager) Devices() []*Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// Get returns the device named by its ID, its current adb serial, or an
// adb serial it was last reached at (nil if
// unknown).
func (m *Manager) Get(name string) *Device {
	m.mu.Lock()
	if id, ok := m.addrs[name]; ok && m.devices[id] != nil {
		d := m.devices[id]
		m.mu.Unlock()
		return d
	}
	if d := m.devices[name]; d != nil {
		m.mu.Unlock()
		return d
	}
	keys := make([]string, 0, len(m.devices))
	for k := range m.devices {
		keys = append(keys, k)
	}
	devs := m.devices
	sort.Strings(keys)
	cand := make([]*Device, 0, len(keys))
	for _, k := range keys {
		cand = append(cand, devs[k])
	}
	m.mu.Unlock()
	var fallback *Device
	for _, d := range cand {
		if d.Matches(name) {
			if d.Owned() || d.Status().Owner != nil {
				return d
			}
			if fallback == nil {
				fallback = d
			}
		}
	}
	return fallback
}

// ReplaySpan is the longest time span the log buffer of a device recorded
// in this run holds (newest - oldest line of its main buffer, measured
// at every (re)connect); 0 if none was measured.
func (m *Manager) ReplaySpan() time.Duration {
	var max time.Duration
	for _, d := range m.Devices() {
		if v := time.Duration(d.spanNs.Load()); v > max {
			max = v
		}
	}
	return max
}

// AddrOf returns the device ID an adb serial resolved to ("" if unknown).
func (m *Manager) AddrOf(addr string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addrs[addr]
}

// Close stops the recorders, waits (bounded) until each has flushed its
// last lines and closes the stores. Attach does nothing afterwards.
func (m *Manager) Close() {
	m.attachMu.Lock()
	m.closed = true
	m.attachMu.Unlock()
	devs := m.Devices()
	for _, d := range devs {
		d.stop()
	}
	done := make(chan struct{})
	go func() {
		for _, d := range devs {
			d.runWG.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Print("recorders did not stop within 10 s; closing the stores anyway")
	}
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	for _, d := range m.Devices() {
		d.Store.Close()
		d.owner.Release()
	}
}
