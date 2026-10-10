package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
}

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
	t.Setenv("ANDROID_LOG_MCP_ADB", adb)
	t.Setenv(fakeadb.EnvDir, dir)
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
	m.Lazy = true
	m.IdleTick = 20 * time.Millisecond
	m.Linger = func(*device.Device) time.Duration { return time.Minute }
	pins := &fakePins{until: map[string]time.Time{}}
	m.PinnedUntil = pins.Until
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	return &lazyEnv{e: e, m: m, tools: &Tools{M: m, Pins: pins}, pins: pins}
}

func (l *lazyEnv) call(t *testing.T, name, args string) (string, bool) {
	t.Helper()
	return l.tools.Call(context.Background(), name, json.RawMessage(args))
}

// The first log tool call starts its device and returns what was in the
// buffer before the call (backfill); nothing ran adb before it.
func TestFirstCallBackfills(t *testing.T) {
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
	l := newLazy(t, 2, "FAKEA", "FAKEB")
	out, isErr := l.call(t, "logcat_status", `{}`)
	if isErr || strings.Contains(out, "cursor (now") || !strings.Contains(out, "2 devices are connected; pass device=one of: FAKEA, FAKEB") || !strings.Contains(out, "No device was started") {
		t.Fatalf("status:\n%s", out)
	}
	if l.m.Wanted("FAKEA") || l.m.Wanted("FAKEB") || len(l.e.CallsMatching("logcat -b")) != 0 {
		t.Fatal("a device was started")
	}
	if _, isErr := l.call(t, "logcat_search", `{}`); !isErr {
		t.Fatal("search without device= succeeded with two devices")
	}
	out, isErr = l.call(t, "logcat_status", `{"device":"FAKEB"}`)
	if isErr || !strings.Contains(out, "cursor (now, FAKEB)") {
		t.Fatalf("status FAKEB:\n%s", out)
	}
	if l.m.Wanted("FAKEA") || !l.m.Wanted("FAKEB") {
		t.Fatal("wrong device started")
	}
}

// No cursor while the backfill runs; the response says to call again.
func TestStatusNoCursorDuringBackfill(t *testing.T) {
	l := newLazy(t, 40, "FAKEA")
	l.m.Track(10 * time.Second) // identify the device before the wait is shortened
	old := device.CatchUpWait
	device.CatchUpWait = 500 * time.Millisecond
	t.Cleanup(func() { device.CatchUpWait = old })
	l.e.SetDelay("FAKEA", 100*time.Millisecond) // ~4 s dump
	out, _ := l.call(t, "logcat_status", `{}`)
	if strings.Contains(out, "cursor (now") || !strings.Contains(out, "backfill in progress (") || !strings.Contains(out, "call logcat_status again") {
		t.Fatalf("status during backfill:\n%s", out)
	}
	out, _ = l.call(t, "logcat_search", `{}`)
	if !strings.Contains(out, "note: backfill in progress (") || !strings.Contains(out, "results may be incomplete, call again") {
		t.Fatalf("search during backfill:\n%s", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ = l.call(t, "logcat_status", `{}`)
		if strings.Contains(out, "cursor (now, FAKEA)") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cursor after the backfill:\n%s", out)
		}
	}
	if !strings.Contains(out, "caught up") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestRecordFor(t *testing.T) {
	l := newLazy(t, 1, "FAKEA")
	for _, bad := range []string{"30", "-1h", "25h", "1d", "abc", "30 m"} {
		out, isErr := l.call(t, "logcat_status", `{"record_for":"`+bad+`"}`)
		if !isErr {
			t.Errorf("record_for %q accepted:\n%s", bad, out)
		}
		if bad == "25h" && !strings.Contains(out, "limit of 24h") {
			t.Errorf("25h: %s", out)
		}
	}
	if l.m.Wanted("FAKEA") {
		t.Fatal("an invalid record_for started recording")
	}
	out, isErr := l.call(t, "logcat_status", `{"record_for":"1h30m"}`)
	if isErr || !strings.Contains(out, "pinned: FAKEA records until") || !l.pins.Until("FAKEA").After(time.Now().Add(89*time.Minute)) {
		t.Fatalf("pin:\n%s", out)
	}
	out, isErr = l.call(t, "logcat_status", `{"record_for":"0"}`)
	if isErr || !strings.Contains(out, "stopped: FAKEA") || !l.pins.Until("FAKEA").IsZero() {
		t.Fatalf("stop:\n%s", out)
	}
	if l.m.Wanted("FAKEA") {
		t.Fatal("record_for=0 did not stop recording")
	}
}

func TestParsePin(t *testing.T) {
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
	l := newLazy(t, 5, "FAKEA")
	l.e.SetIdentDelay("FAKEA", 3*time.Second)
	l.m.Track(0) // adb lists it; its identity takes 3 s
	for end := time.Now().Add(10 * time.Second); len(l.m.Identifying()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("device never listed")
		}
	}
	old := device.CatchUpWait
	device.CatchUpWait = 300 * time.Millisecond
	t.Cleanup(func() { device.CatchUpWait = old })
	out, isErr := l.call(t, "logcat_status", `{}`)
	if isErr || !strings.Contains(out, "adb lists FAKEA but reading its identity has not finished yet") || strings.Contains(out, "cursor (now") {
		t.Fatalf("status:\n%s", out)
	}
	if out, isErr := l.call(t, "logcat_search", `{}`); !isErr || !strings.Contains(out, "call again") {
		t.Fatalf("search (error %v):\n%s", isErr, out)
	}
	device.CatchUpWait = 10 * time.Second
	if out, _ := l.call(t, "logcat_status", `{}`); !strings.Contains(out, "cursor (now, FAKEA)") {
		t.Fatalf("status after identification:\n%s", out)
	}
}
