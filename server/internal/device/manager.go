package device

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Manager tracks adb devices and their stores.
type Manager struct {
	Base       string
	Cfg        store.Config
	MaxDevices int
	// NoADB disables adb entirely (tests, or browsing recorded history only).
	NoADB bool

	adb      *ADB
	adbErr   error
	ctx      context.Context
	mu       sync.Mutex
	devices  map[string]*Device
	states   map[string]string // live adb states (incl. unauthorized/offline)
	watchErr error
	ready    chan struct{}
	once     sync.Once
}

// DefaultBase is os.UserCacheDir()/android-log-mcp (override: ANDROID_LOG_MCP_HOME).
func DefaultBase() string {
	if v := os.Getenv("ANDROID_LOG_MCP_HOME"); v != "" {
		return v
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "android-log-mcp")
}

func NewManager(base string, cfg store.Config) *Manager {
	m := &Manager{Base: base, Cfg: cfg, MaxDevices: 4, devices: map[string]*Device{}, states: map[string]string{}, ready: make(chan struct{})}
	if v, err := strconv.Atoi(os.Getenv("ANDROID_LOG_MCP_MAX_DEVICES")); err == nil && v > 0 {
		m.MaxDevices = v
	}
	return m
}

// Start begins device tracking (non-blocking).
func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	os.MkdirAll(m.Base, 0o755)
	// previously recorded devices are queryable even when offline
	for _, dir := range store.ListDeviceDirs(m.Base) {
		st, err := store.OpenReadOnly(dir, m.Cfg)
		if err != nil || st.Serial == "" {
			continue
		}
		m.mu.Lock()
		if _, ok := m.devices[st.Serial]; !ok {
			// reopen properly (may become writer later when the device shows up)
			if s2, err := store.Open(m.Base, st.Serial, m.Cfg); err == nil {
				m.devices[st.Serial] = newDevice(nil, s2, st.Serial)
			}
		}
		m.mu.Unlock()
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
		sleep(ctx, 3*time.Second)
		m.once.Do(func() { close(m.ready) })
	}()
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
	seen := map[string]bool{}
	m.states = map[string]string{}
	var start []*Device
	var stop []*Device
	recording := 0
	for _, d := range m.devices {
		if d.Status().State == "device" {
			recording++
		}
	}
	for _, ds := range list {
		seen[ds.Serial] = true
		m.states[ds.Serial] = ds.State
		d := m.devices[ds.Serial]
		if ds.State == "device" && d == nil {
			if recording >= m.MaxDevices {
				continue
			}
			st, err := store.Open(m.Base, ds.Serial, m.Cfg)
			if err != nil {
				continue
			}
			d = newDevice(m.adb, st, ds.Serial)
			m.devices[ds.Serial] = d
			recording++
		}
		if d != nil {
			start = append(start, d)
		}
	}
	for s, d := range m.devices {
		if !seen[s] {
			stop = append(stop, d)
		}
	}
	m.mu.Unlock()
	for _, d := range start {
		d.setState(m.ctx, m.states[d.Serial])
	}
	for _, d := range stop {
		d.setState(m.ctx, "")
	}
	m.once.Do(func() { close(m.ready) })
}

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

// Get returns the device for serial (nil if unknown).
func (m *Manager) Get(serial string) *Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.devices[serial]
}

// Close stops recorders and flushes stores.
func (m *Manager) Close() {
	for _, d := range m.Devices() {
		d.stop()
	}
	time.Sleep(200 * time.Millisecond)
	for _, d := range m.Devices() {
		d.Store.Close()
	}
}
