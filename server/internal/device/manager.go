package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Manager tracks adb devices and their stores. Devices are keyed by their
// stable ID (ResolveID); the adb serial is only the connection address.
type Manager struct {
	// Base holds device data: the per-user root, or ANDROID_LOG_MCP_HOME.
	Base string
	// Root is the per-user root; its owners/ lock decides which process
	// records a device, whatever Base is.
	Root       layout.Root
	Version    string
	Cfg        store.Config
	MaxDevices int
	// NoADB disables adb entirely (tests, or browsing recorded history only).
	NoADB bool
	// MaxTotalBytes caps everything under Base (all devices and formats,
	// legacy data, tmp/, run/); 0 disables. Default 2 GiB.
	MaxTotalBytes int64
	// Disk pauses recording while free space on the data disk is low.
	Disk *store.DiskGuard
	// KeepLegacy (ANDROID_LOG_MCP_LEGACY=keep): do not migrate, move or
	// expire data of older versions (flat <serial>/ dirs, android-log-mcp.*
	// siblings); it stays readable and counts towards the total cap.
	KeepLegacy bool

	adb       *ADB
	adbErr    error
	ctx       context.Context
	mu        sync.Mutex
	devices   map[string]*Device // by device ID (flat legacy dirs: by their stored serial)
	states    map[string]string  // live adb states by adb serial (incl. unauthorized/offline)
	addrs     map[string]string  // adb serial -> device ID
	resolving map[string]bool
	attachMu  sync.Mutex // serializes attach / migration / sweep
	watchErr  error
	ready     chan struct{}
	once      sync.Once
}

// DefaultBase is ANDROID_LOG_MCP_HOME if set, else the per-user root
// os.UserCacheDir()/android-log-mcp.
func DefaultBase() string {
	if v := os.Getenv("ANDROID_LOG_MCP_HOME"); v != "" {
		return v
	}
	return layout.DefaultDir()
}

func NewManager(base string, cfg store.Config) *Manager {
	m := &Manager{Base: base, Root: layout.Root{Dir: layout.DefaultDir()}, Cfg: cfg, MaxDevices: 4,
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
	switch v := os.Getenv("ANDROID_LOG_MCP_LEGACY"); v {
	case "", "migrate":
	case "keep":
		m.KeepLegacy = true
	default:
		log.Printf("ANDROID_LOG_MCP_LEGACY=%q: expected keep or migrate; using migrate", v)
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

// homeMode: data lives outside the per-user root (ANDROID_LOG_MCP_HOME).
func (m *Manager) homeMode() bool { return !samePath(m.Base, m.Root.Dir) }

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if ea, err := filepath.EvalSymlinks(aa); err == nil {
		aa = ea
	}
	if eb, err := filepath.EvalSymlinks(bb); err == nil {
		bb = eb
	}
	return aa == bb
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
			d.owner.Touch() // non-flock platforms: keep the owner lock fresh
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
// under the data root: other devices, old formats, legacy data, tmp/, run/,
// the total size cap) and, with aux, compacts the auxiliary files of the
// devices this process writes.
func (m *Manager) MaintainOnce(aux bool) {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
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
	res := layout.Sweep(layout.SweepOptions{Base: m.Base, Root: &m.Root, MaxTotal: m.MaxTotalBytes, MaxAge: m.maxAge(), Owned: owned, KeepLegacy: m.KeepLegacy})
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
	os.MkdirAll(m.Base, 0o755)
	if !m.NoADB {
		// startup sweep (--offline only browses, e.g. an unzipped report:
		// never move, prune or delete anything there)
		if err := m.Root.Ensure(); err != nil {
			log.Printf("data root %s: %v", m.Root.Dir, err)
		}
		os.MkdirAll(store.DataDir(m.Base), 0o755)
		if !m.homeMode() && !m.KeepLegacy {
			for _, d := range m.Root.AdoptSiblings() {
				log.Printf("moved %s into %s (expires with the retention window)", filepath.Base(d), d)
			}
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
	go m.adb.Watch(ctx, m.onDevices)
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
	m.attach(id)
	m.mu.Lock()
	delete(m.resolving, addr)
	if m.states[addr] != "" {
		m.addrs[addr] = id.ID
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
	if f.Legacy {
		return store.WriterLockHeld(f.DataDir)
	}
	return m.Root.OwnerHeld(id)
}

// legacyWriter finds a pre-v1 version recording addr into a flat directory.
func (m *Manager) legacyWriter(addr string) *layout.Owner {
	bases := []string{m.Base}
	if m.homeMode() {
		bases = append(bases, m.Root.Dir)
	}
	for _, b := range bases {
		dir := filepath.Join(b, store.SanitizeSerial(addr))
		if store.WriterLockHeld(dir) {
			o := &layout.Owner{DataDir: dir, Addr: addr, Version: "pre-0.4 (flat layout)", Legacy: true}
			if bs, err := os.ReadFile(filepath.Join(dir, "writer.lock")); err == nil {
				o.PID, _ = strconv.Atoi(strings.TrimSpace(string(bs)))
			}
			return o
		}
	}
	return nil
}

// Attach is attach for a device whose identity is already known (tests,
// tools that probed it themselves).
func (m *Manager) Attach(id Identity) { m.attach(id) }

// attach decides who records device id: this process (taking the global
// owner lock, migrating a matching flat directory, recording into
// <base>/v1/<id>) or another one (follow its data read-only and report it).
func (m *Manager) attach(id Identity) {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	if m.ctx != nil && m.ctx.Err() != nil {
		return
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
			return
		}
	}
	if o := m.legacyWriter(id.Addr); o != nil {
		m.follow(id, o)
		return
	}
	owned := 0
	for _, d := range m.Devices() {
		if d.Owned() {
			owned++
		}
	}
	if owned >= m.MaxDevices {
		log.Printf("%s (%s): not recorded, already recording %d devices (ANDROID_LOG_MCP_MAX_DEVICES)", id.ID, id.Addr, owned)
		return
	}
	dir := layout.DeviceDir(m.Base, id.ID)
	info := layout.Owner{PID: os.Getpid(), Version: m.Version, DataDir: dir, Addr: id.Addr, Since: time.Now().UTC()}
	if m.homeMode() {
		info.Home = m.Base
	}
	lock, other, err := m.Root.AcquireOwner(id.ID, info)
	if errors.Is(err, layout.ErrOwned) {
		if other == nil {
			other = &layout.Owner{}
		}
		m.follow(id, other)
		return
	}
	if err != nil {
		log.Printf("%s: owner lock: %v", id.ID, err)
		return
	}
	migratedFrom, oldSerial := m.migrate(id, dir)
	st, err := store.OpenDir(dir, id.ID, m.Cfg, true)
	if err != nil {
		lock.Release()
		log.Printf("%s: open %s: %v", id.ID, dir, err)
		return
	}
	st.UpdateMeta(func(mt *store.Meta) {
		mt.Serial, mt.Addr, mt.IDSource = id.ID, id.Addr, id.Source
		for _, a := range []string{oldSerial, id.Addr} {
			if a != "" && a != id.ID && !contains(mt.Aliases, a) {
				mt.Aliases = append(mt.Aliases, a)
			}
		}
	})
	d := m.newDevice(st, id.ID)
	d.addr = id.Addr
	d.owner = lock
	d.adb = m.adb
	m.swap(id.ID, d, migratedFrom)
	if migratedFrom != "" {
		log.Printf("%s: migrated %s -> %s", id.ID, migratedFrom, dir)
	}
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
		cur.foreign = o
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
	d.adb = m.adb
	m.swap(id.ID, d, "")
	log.Printf("%s (%s): recorded by another android-log-mcp process (%s); following read-only", id.ID, id.Addr, o)
}

// swap installs d under key, retiring the previous device with that key and
// any other entry reading the same directory (or the migrated one).
func (m *Manager) swap(key string, d *Device, movedFrom string) {
	m.mu.Lock()
	var old []*Device
	for k, e := range m.devices {
		if k == key || e.Store.Dir == d.Store.Dir || (movedFrom != "" && e.Store.Dir == movedFrom) {
			delete(m.devices, k)
			if e.Store.Dir != d.Store.Dir && e.Store.Dir != movedFrom && exists(e.Store.Dir) && !e.Owned() && e.Status().Owner == nil {
				// unrelated legacy data that happened to use this key: keep it browsable
				rel, _ := filepath.Rel(m.Base, e.Store.Dir)
				nk := k + "@" + filepath.ToSlash(rel)
				e.Serial = nk
				m.devices[nk] = e
				continue
			}
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

// migrate moves the pre-v1 flat directory of this device into dir. A flat
// directory belongs to the device when its stored serial is the device's
// ro.serialno (physical devices over USB), or when it recorded the
// device's current boot (boot IDs are random per boot) — the adb serial
// alone is not enough (emulator ports and ip:port are reused). Returns the
// old directory and its stored serial ("" if nothing was migrated).
func (m *Manager) migrate(id Identity, dir string) (string, string) {
	if m.KeepLegacy || exists(filepath.Join(dir, "meta.json")) {
		return "", "" // opted out, or already has v1 data (old data expires as legacy)
	}
	var best string
	var bestMeta store.Meta
	for _, flat := range store.ListFlatDirs(m.Base) {
		mt, ok := readMeta(flat)
		if !ok {
			continue
		}
		match := (id.Source == "ro.serialno" && mt.Serial == id.ID) || (id.BootID != "" && hasBoot(mt, id.BootID))
		if match && (best == "" || mt.LastTimeNs > bestMeta.LastTimeNs) {
			best, bestMeta = flat, mt
		}
	}
	if best == "" {
		return "", ""
	}
	f, ok := store.TryLock(filepath.Join(best, "writer.lock"))
	if !ok {
		return "", "" // still recorded by an old version
	}
	// our own read-only view of it must not keep files open (Windows
	// cannot rename a directory with open files); swap drops the entry
	m.mu.Lock()
	for _, e := range m.devices {
		if e.Store.Dir == best {
			e.Store.Close()
		}
	}
	m.mu.Unlock()
	os.Remove(filepath.Join(best, "writer.lock")) // unix: while holding it
	store.ReleaseLock(f)
	os.Remove(filepath.Join(best, "writer.lock"))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", ""
	}
	os.Remove(dir) // an empty leftover
	if err := os.Rename(best, dir); err != nil {
		log.Printf("%s: migrating %s: %v (left in place; expires as legacy data)", id.ID, best, err)
		return "", ""
	}
	return best, bestMeta.Serial
}

func readMeta(dir string) (store.Meta, bool) {
	var mt store.Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(b, &mt) != nil {
		return mt, false
	}
	return mt, true
}

func hasBoot(mt store.Meta, boot string) bool {
	if mt.LastBootID == boot {
		return true
	}
	for _, s := range mt.Sessions {
		if s.BootID == boot {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
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
// alias such as the adb serial of a migrated pre-v1 directory (nil if
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

// AddrOf returns the device ID an adb serial resolved to ("" if unknown).
func (m *Manager) AddrOf(addr string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addrs[addr]
}

// Close stops recorders and flushes stores.
func (m *Manager) Close() {
	for _, d := range m.Devices() {
		d.stop()
	}
	time.Sleep(200 * time.Millisecond)
	for _, d := range m.Devices() {
		d.Store.Close()
		d.owner.Release()
	}
}
