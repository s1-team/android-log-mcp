package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// The tools with lazy recording (issue #11) against a fake adb.

type lazyEnv struct {
	e     fakeadb.Env
	m     *device.Manager
	tools *Tools
	pins  *fakePins
	// linger is the idle linger (default 1 min); tests may change it
	linger *atomic.Int64
}

// lazyWait bounds the waits on fake adb (a process per call, run under -race
// with the package's tests in parallel): it only limits a hang, waitUntil
// returns as soon as the condition holds (#158).
const lazyWait = 60 * time.Second

type fakePins struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (p *fakePins) Set(id string, d time.Duration, by string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	dl := time.Now().Add(d)
	if dl.After(p.until[id]) {
		p.until[id] = dl
	}
	return p.until[id], nil
}
func (p *fakePins) Stop(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.until, id)
	return nil
}
func (p *fakePins) Until(id string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.until[id]
}
func (p *fakePins) Describe(id string, loc *time.Location) string {
	if u := p.Until(id); !u.IsZero() {
		return "pinned until " + u.Format(time.DateTime)
	}
	return ""
}

func newLazy(t *testing.T, buffered int, serials ...string) *lazyEnv {
	t.Helper()
	dir := t.TempDir()
	adb, err := fakeadb.Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := fakeadb.Env{Dir: dir}
	states := map[string]string{}
	for _, s := range serials {
		if err := e.AddDevice(s, "boot-"+s); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		var lines []string
		for i := 0; i < buffered; i++ {
			lines = append(lines, fakeadb.Line(now.Add(time.Duration(i-buffered)*10*time.Millisecond), 100, "LAZYPRE", fmt.Sprintf("%s pre %d", s, i)))
		}
		e.Log(s, lines...)
		states[s] = "device"
	}
	e.SetDevices(states)
	m := device.NewManager(t.TempDir(), store.Config{})
	m.ADBPath = adb
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	linger := new(atomic.Int64)
	linger.Store(int64(time.Minute))
	m.Linger = func(*device.Device) time.Duration { return time.Duration(linger.Load()) }
	pins := &fakePins{until: map[string]time.Time{}}
	m.PinnedUntil = pins.Until
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	return &lazyEnv{e: e, m: m, tools: &Tools{M: m, Pins: pins}, pins: pins, linger: linger}
}

func (l *lazyEnv) call(t *testing.T, name, args string) (string, bool) {
	t.Helper()
	return l.tools.Call(context.Background(), name, json.RawMessage(args))
}

// The first log tool call starts its device and returns what was in the
// buffer before the call (backfill); nothing ran adb before it.
func TestFirstCallBackfills(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 5, "FAKEA")
	time.Sleep(200 * time.Millisecond)
	if c := l.e.Calls(); len(c) != 0 {
		t.Fatalf("adb ran before the first call: %q", c)
	}
	out, isErr := l.call(t, "logcat_search", `{"filter":"tag=:LAZYPRE"}`)
	if isErr || !strings.Contains(out, "FAKEA pre 0") || !strings.Contains(out, "FAKEA pre 4") || strings.Contains(out, "backfill in progress") {
		t.Fatalf("search:\n%s", out)
	}
	if !l.m.Wanted("FAKEA") {
		t.Fatal("not started")
	}
}

// With several online devices and no device=, logcat_status lists them as
// not recorded, starts none and returns no cursor; naming one starts only
// that one.
func TestStatusSeveralDevicesStartsNone(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 2, "FAKEA", "FAKEB")
	out, isErr := l.call(t, "logcat_status", `{}`)
	if isErr || !strings.Contains(out, " cursor=none") || !strings.Contains(out, "hint code=several_devices devices=FAKEA,FAKEB started=0 next: device=\"FAKEA\" | device=\"FAKEB\"") {
		t.Fatalf("status:\n%s", out)
	}
	if l.m.Wanted("FAKEA") || l.m.Wanted("FAKEB") || len(l.e.CallsMatching("logcat -b")) != 0 {
		t.Fatal("a device was started")
	}
	if _, isErr := l.call(t, "logcat_search", `{}`); !isErr {
		t.Fatal("search without device= succeeded with two devices")
	}
	out, isErr = l.call(t, "logcat_status", `{"device":"FAKEB"}`)
	if isErr || cursorOf(out) == "" {
		t.Fatalf("status FAKEB:\n%s", out)
	}
	if l.m.Wanted("FAKEA") || !l.m.Wanted("FAKEB") {
		t.Fatal("wrong device started")
	}
}

// No cursor while the backfill runs; the response says to call again.
func TestStatusNoCursorDuringBackfill(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 40, "FAKEA")
	l.m.Track(10 * time.Second) // identify the device before the wait is shortened
	l.m.CatchUpWait = 500 * time.Millisecond
	l.e.SetDelay("FAKEA", 100*time.Millisecond) // ~4 s dump
	out, _ := l.call(t, "logcat_status", `{}`)
	if cursorOf(out) != "" || !strings.Contains(out, " caught_up=0 ") || !strings.Contains(out, "note code=backfill backfill=") {
		t.Fatalf("status during backfill:\n%s", out)
	}
	out, _ = l.call(t, "logcat_search", `{}`)
	if !strings.Contains(out, "\nnote code=backfill backfill=running lines=") || !strings.Contains(out, " results=incomplete next: logcat_status") || !(strings.HasPrefix(out, "ok ") || strings.HasPrefix(out, "empty ")) {
		t.Fatalf("search during backfill:\n%s", out)
	}
	if strings.HasPrefix(out, "empty ") && (!strings.Contains(out, " matched=0 scanned=") || !strings.Contains(out, "next:")) {
		t.Fatalf("empty search during backfill lacks matched=0 scanned= or next:\n%s", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ = l.call(t, "logcat_status", `{}`)
		if cursorOf(out) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cursor after the backfill:\n%s", out)
		}
	}
	if !strings.Contains(out, " caught_up=1 ") {
		t.Fatalf("status:\n%s", out)
	}
	if out, _ = l.call(t, "logcat_search", `{}`); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("search after the backfill:\n%s", out)
	}
}

func TestRecordFor(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 1, "FAKEA")
	for _, bad := range []string{"30", "-1h", "25h", "1d", "abc", "30 m"} {
		out, isErr := l.call(t, "logcat_status", `{"record_for":"`+bad+`"}`)
		if !isErr {
			t.Errorf("record_for %q accepted:\n%s", bad, out)
		}
		if bad == "25h" && !strings.Contains(out, "valid=\"30m|2h|1h30m|0 (max 24h)\"") {
			t.Errorf("25h: %s", out)
		}
	}
	if l.m.Wanted("FAKEA") {
		t.Fatal("an invalid record_for started recording")
	}
	out, isErr := l.call(t, "logcat_status", `{"record_for":"1h30m"}`)
	if isErr || !strings.Contains(out, "pinned device=FAKEA until=") || !l.pins.Until("FAKEA").After(time.Now().Add(89*time.Minute)) {
		t.Fatalf("pin:\n%s", out)
	}
	out, isErr = l.call(t, "logcat_status", `{"record_for":"0"}`)
	if isErr || !strings.Contains(out, "stopped device=FAKEA pin=removed") || !l.pins.Until("FAKEA").IsZero() {
		t.Fatalf("stop:\n%s", out)
	}
	if l.m.Wanted("FAKEA") {
		t.Fatal("record_for=0 did not stop recording")
	}
}

func TestParsePin(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "2h": 2 * time.Hour, "90m": 90 * time.Minute, "1h30m": 90 * time.Minute, "24h": 24 * time.Hour, "0": 0, "45s": 45 * time.Second} {
		if got, err := ParsePin(in); err != nil || got != want {
			t.Errorf("ParsePin(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "30", "1.5h", "24h1m", "-30m", "0m", "2d", "1H"} {
		if _, err := ParsePin(in); err == nil {
			t.Errorf("ParsePin(%q) accepted", in)
		}
	}
}

func TestToolDescriptionsAndSchemas(t *testing.T) {
	t.Parallel()
	for _, d := range ToolDefs() {
		var v map[string]any
		if err := json.Unmarshal(d.Schema, &v); err != nil {
			t.Errorf("%s schema: %v", d.Name, err)
		}
		if d.Name == "logcat_status" {
			if !strings.HasPrefix(d.Desc, "Starts recording the device, or extends it with record_for (max 24h; \"0\" stops).") {
				t.Errorf("status description must start with the side effect: %q", d.Desc)
			}
			if !strings.Contains(d.Desc, "If the response says backfill in progress, call logcat_status again before using a cursor") {
				t.Errorf("status description lacks the retry instruction")
			}
		}
	}
	if !strings.Contains(Instructions(), "Recording starts on use") {
		t.Errorf("instructions: %s", Instructions())
	}
}

// A device whose identity is still being read when the wait ends: the call
// says so and asks to call again instead of "no device connected".
func TestSlowIdentity(t *testing.T) {
	t.Parallel()
	l := newLazy(t, 5, "FAKEA")
	l.e.SetIdentDelay("FAKEA", 3*time.Second)
	l.m.Track(0) // adb lists it; its identity takes 3 s
	waitUntil(t, "the device to be listed", lazyWait, func() bool { return len(l.m.Identifying()) != 0 })
	l.m.CatchUpWait = 300 * time.Millisecond
	out, isErr := l.call(t, "logcat_status", `{}`)
	if isErr || !strings.Contains(out, "code=device_identifying devices=FAKEA") || cursorOf(out) != "" {
		t.Fatalf("status:\n%s", out)
	}
	if out, isErr := l.call(t, "logcat_search", `{}`); !isErr || !strings.Contains(out, "error code=device_identifying") || !strings.Contains(out, "retry_after_s=3 next: logcat_status") {
		t.Fatalf("search (error %v):\n%s", isErr, out)
	}
	l.m.CatchUpWait = device.DefaultCatchUpWait
	if out, _ := l.call(t, "logcat_status", `{}`); cursorOf(out) == "" {
		t.Fatalf("status after identification:\n%s", out)
	}
}

// Issue #30: a reconnect whose previous recording session does not stop in
// time starts recording anyway, and logcat_status says so explicitly.
func TestStatusShowsLateHandover(t *testing.T) {
	// serial: device.HoldSessionEndForTest sets a package-wide hook (sessionEndHook)
	l := newLazy(t, 3, "FAKEA")
	if out, isErr := l.call(t, "logcat_status", `{}`); isErr {
		t.Fatal(out)
	}
	release := device.HoldSessionEndForTest(l.m, "FAKEA", 1, 200*time.Millisecond)
	defer release()
	l.e.SetDevices(map[string]string{"FAKEA": "offline"})
	deadline := time.Now().Add(20 * time.Second)
	for l.m.Get("FAKEA").Status().State != "offline" {
		if time.Now().After(deadline) {
			t.Fatal("disconnect not seen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.e.SetDevices(map[string]string{"FAKEA": "device"})
	var out string
	for {
		out, _ = l.call(t, "logcat_status", `{}`)
		if strings.Contains(out, "note code=handoff") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logcat_status does not show the late handover:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("logcat_status:\n%s", out)
	if !strings.Contains(out, "recording session 2 started at") {
		t.Fatalf("logcat_status:\n%s", out)
	}
}

// Issue #109: a service process that replaced another one (version handoff,
// hung service process) has no wanted devices; the first call naming a device whose
// stored data exists must start it even while the device list or its
// identity is still pending, not answer idle / not connected from the
// stored data.
func TestNamedStoredDeviceStartsWhileIdentityPending(t *testing.T) {
	l := newLazy(t, 5, "FAKEA")
	if out, isErr := l.call(t, "logcat_status", `{"device":"FAKEA"}`); isErr || !strings.Contains(out, "FAKEA") {
		t.Fatalf("first service:\n%s", out)
	}
	// the old service process goes away; the new one finds the device slow
	l.m.Close()
	l.e.SetIdentDelay("FAKEA", 3*time.Second)
	m := device.NewManager(l.m.Base, store.Config{})
	m.ADBPath = l.m.ADBPath
	m.CatchUpWait = 300 * time.Millisecond
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	m.Linger = func(*device.Device) time.Duration { return time.Minute }
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	tools := &Tools{M: m, Pins: l.pins}

	out, _ := tools.Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"FAKEA"}`))
	if strings.Contains(out, "recording: idle") || strings.Contains(out, "not recording") || strings.Contains(out, "device not connected") {
		t.Fatalf("answered from stored data:\n%s", out)
	}
	if !m.Wanted("FAKEA") {
		t.Fatalf("device not started:\n%s", out)
	}
	if out, isErr := tools.Call(context.Background(), "logcat_search", json.RawMessage(`{"device":"FAKEA"}`)); isErr {
		t.Fatalf("search (error):\n%s", out)
	}
	// once the identity is read, fakeadb sees logcat start for the device
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fakeadb calls: %q", l.e.Calls())
		}
	})
	waitUntil(t, "fakeadb to see logcat start for FAKEA", lazyWait, func() bool { return l.e.LogcatRunning("FAKEA") && len(l.e.CallsMatching("logcat")) != 0 })
}

// A name that matches no stored data and no listed device still starts
// nothing while an identity is pending.
func TestUnknownNameDoesNotStartWhileIdentityPending(t *testing.T) {
	l := newLazy(t, 5, "FAKEA")
	l.e.SetIdentDelay("FAKEA", 3*time.Second)
	l.m.Track(0)
	waitUntil(t, "the device to be listed", lazyWait, func() bool { return len(l.m.Identifying()) != 0 })
	l.m.CatchUpWait = 300 * time.Millisecond
	l.call(t, "logcat_status", `{"device":"NOSUCH"}`)
	if ids := l.m.WantedIDs(); len(ids) != 0 {
		t.Fatalf("started %v for an unknown name", ids)
	}
}

// Issue #109: a device never recorded before, named by its adb serial, is
// started by the first call when its identity is still pending.
func TestNamedAdbSerialStartsWhileIdentityPending(t *testing.T) {
	l := newLazy(t, 5, "FAKEB")
	l.e.SetIdentDelay("FAKEB", 2*time.Second)
	l.m.Track(0)
	waitUntil(t, "the device to be listed", lazyWait, func() bool { return len(l.m.Identifying()) != 0 })
	l.m.CatchUpWait = 300 * time.Millisecond
	l.call(t, "logcat_status", `{"device":"FAKEB"}`)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fakeadb calls: %q", l.e.Calls())
		}
	})
	waitUntil(t, "FAKEB to start after its identity was read", lazyWait, func() bool { return l.m.Wanted("FAKEB") && l.e.LogcatRunning("FAKEB") })
}
