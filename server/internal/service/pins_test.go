package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

func TestSetPinNeverShortens(t *testing.T) {
	t.Parallel()
	now := time.Now()
	pf := &PinFile{Pins: map[string]*Pin{}}
	p := pf.SetPin("d", 2*time.Hour, "a", "0.5.0", now)
	if !p.Deadline.Equal(now.Add(2 * time.Hour)) {
		t.Fatal(p.Deadline)
	}
	p = pf.SetPin("d", 30*time.Minute, "b", "0.5.0", now.Add(time.Minute))
	if !p.Deadline.Equal(now.Add(2*time.Hour)) || p.SetBy != "b" {
		t.Fatalf("shortened: %+v", p)
	}
	p = pf.SetPin("d", 3*time.Hour, "c", "0.5.0", now.Add(time.Minute))
	if !p.Deadline.Equal(now.Add(time.Minute + 3*time.Hour)) {
		t.Fatalf("not extended: %+v", p)
	}
}

func TestPinsFile(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	os.MkdirAll(ctrl.Run(), 0o700)
	now := time.Now()
	// written atomically with "v": 1
	pf := &PinFile{Pins: map[string]*Pin{}}
	pf.SetPin("a", time.Hour, "t", "0.5.0", now)
	pf.Pins["past"] = &Pin{Deadline: now.Add(-time.Minute)}
	pf.Pins["far"] = &Pin{Deadline: now.Add(48 * time.Hour)}
	if err := WritePins(ctrl, pf); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ctrl.PinsPath())
	if !strings.Contains(string(b), `"v": 1`) {
		t.Fatalf("record.json:\n%s", b)
	}
	if _, err := os.Stat(ctrl.PinsPath() + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
	got, warn, err := ReadPins(ctrl, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Pins["past"]; ok {
		t.Error("past pin kept")
	}
	if dl := got.Pins["far"].Deadline; !dl.Equal(now.Add(MaxPin)) {
		t.Errorf("far deadline %v not clamped", dl)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "clamped") {
		t.Errorf("warnings %q", warn)
	}
	// unknown version / garbage: ignored, never overwritten, pinning refused
	for _, content := range []string{`{"v": 2, "pins": {}}`, `not json`} {
		os.WriteFile(ctrl.PinsPath(), []byte(content), 0o600)
		if _, _, err := ReadPins(ctrl, now); err == nil {
			t.Fatalf("%q accepted", content)
		}
		if _, err := UpdatePins(ctrl, now, func(pf *PinFile) error { pf.SetPin("x", time.Hour, "t", "0.5.0", now); return nil }); err == nil {
			t.Fatalf("%q overwritten", content)
		}
		if b, _ := os.ReadFile(ctrl.PinsPath()); string(b) != content {
			t.Fatalf("file changed: %s", b)
		}
		if HasActivePin(ctrl, now) {
			t.Fatal("unusable file has an active pin")
		}
	}
}

func TestPinCrashLoop(t *testing.T) {
	t.Parallel()
	now := time.Now()
	pf := &PinFile{Pins: map[string]*Pin{}}
	pf.SetPin("d", time.Hour, "t", "0.5.0", now)
	for i := 0; i < 2; i++ {
		pf.crashed("0.5.0", "boom", now.Add(time.Duration(i)*10*time.Second))
		if !pf.Pins["d"].Active(now) {
			t.Fatalf("suspended after %d crashes", i+1)
		}
	}
	notes := pf.crashed("0.5.0", "boom 3", now.Add(20*time.Second))
	p := pf.Pins["d"]
	if p.Suspended != SuspendedCrashLoop || p.LastError != "boom 3" || p.Active(now) || len(notes) != 1 || !strings.Contains(notes[0], "crash-loop") {
		t.Fatalf("pin %+v notes %q", p, notes)
	}
	if !strings.Contains(p.Describe(now), "SUSPENDED: crash-loop") || !strings.Contains(p.Describe(now), "last error: boom 3") {
		t.Fatalf("describe %q", p.Describe(now))
	}
	// a new pin clears it
	p = pf.SetPin("d", time.Hour, "t", "0.5.0", now.Add(time.Minute))
	if p.Suspended != "" || p.Restarts.Count != 0 || len(p.RecentCrashes) != 0 || !p.Active(now.Add(time.Minute)) {
		t.Fatalf("not cleared: %+v", p)
	}
}

// Slow periodic crashes never trip the 1-minute rule; the budget of 10
// automatic restarts per (version, pin) does. It survives in record.json,
// is kept on a same-version takeover and reset by a different version or
// a new pin.
func TestPinCrashBudget(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	os.MkdirAll(ctrl.Run(), 0o700)
	now := time.Now()
	UpdatePins(ctrl, now, func(pf *PinFile) error { pf.SetPin("d", 23*time.Hour, "t", "0.5.0", now); return nil })
	crash := func(i int, version string) *Pin {
		at := now.Add(time.Duration(i) * 2 * time.Minute)
		pf, err := UpdatePins(ctrl, at, func(pf *PinFile) error {
			pf.takeover(version)
			pf.crashed(version, fmt.Sprintf("crash %d", i), at)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return pf.Pins["d"]
	}
	for i := 1; i <= 10; i++ {
		if p := crash(i, "0.5.0"); p.Suspended != "" || p.Restarts.Count != i {
			t.Fatalf("crash %d: %+v", i, p)
		}
	}
	// a same-version takeover keeps the count
	pf, _ := UpdatePins(ctrl, now.Add(21*2*time.Minute), func(pf *PinFile) error { pf.takeover("0.5.0"); return nil })
	if pf.Pins["d"].Restarts.Count != 10 {
		t.Fatalf("same-version takeover reset the count: %+v", pf.Pins["d"])
	}
	p := crash(11, "0.5.0")
	if p.Suspended != SuspendedCrashBudget || p.LastError != "crash 11" {
		t.Fatalf("11th crash: %+v", p)
	}
	// a newer version takes over: fresh budget (but the suspension stays
	// until a new record --for)
	pf, _ = UpdatePins(ctrl, now.Add(25*2*time.Minute), func(pf *PinFile) error { pf.takeover("0.5.1"); return nil })
	if r := pf.Pins["d"].Restarts; r.Version != "0.5.1" || r.Count != 0 {
		t.Fatalf("newer version: %+v", r)
	}
	at := now.Add(26 * 2 * time.Minute)
	pf, _ = UpdatePins(ctrl, at, func(pf *PinFile) error { pf.SetPin("d", time.Hour, "t", "0.5.1", at); return nil })
	if p := pf.Pins["d"]; p.Suspended != "" || p.Restarts.Count != 0 || !p.Active(at) {
		t.Fatalf("new pin: %+v", p)
	}
}

// A service that finds the previous one's service.json (it did not shut
// down) counts a crash for every active pin; three in a minute suspend it,
// shown by the service status.
func TestServiceCountsCrashes(t *testing.T) {
	t.Parallel()
	ctrl := testControl(t)
	os.MkdirAll(ctrl.Run(), 0o700)
	now := time.Now()
	UpdatePins(ctrl, now, func(pf *PinFile) error { pf.SetPin("dev1", time.Hour, "test", "0.5.0", now); return nil })
	for i := 1; i <= 3; i++ {
		stale := State{PID: deadPID(t), Version: "0.5.0", Started: time.Now().Add(-time.Minute), Root: ctrl.Dir}
		b, _ := json.Marshal(stale)
		os.WriteFile(ctrl.StatePath(), b, 0o600)
		s, err := Start(Options{Control: ctrl, Version: "0.5.0", Drain: time.Second, ExitGrace: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		info := s.Info()
		s.Stop("test")
		pf, _, _ := ReadPins(ctrl, time.Now())
		p := pf.Pins["dev1"]
		if p.Restarts.Count != i || !strings.Contains(p.LastError, "ended without shutting down") {
			t.Fatalf("start %d: %+v", i, p)
		}
		if i == 3 {
			if p.Suspended != SuspendedCrashLoop {
				t.Fatalf("not suspended: %+v", p)
			}
			if !strings.Contains(strings.Join(info.Pins, "\n"), "SUSPENDED: crash-loop") || !strings.Contains(info.Text(), "last error: service pid") {
				t.Fatalf("status:\n%s", info.Text())
			}
		}
	}
	// a clean stop leaves no service.json: the next start is no crash
	s, err := Start(Options{Control: ctrl, Version: "0.5.0", Drain: time.Second, ExitGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.Stop("test")
	pf, _, _ := ReadPins(ctrl, time.Now())
	if pf.Pins["dev1"].Restarts.Count != 3 {
		t.Fatalf("clean restart counted: %+v", pf.Pins["dev1"])
	}
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := procattr.Hide(exec.Command(os.Args[0], "-test.run=^$"))
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// record (the op): --for pins and starts the single online device, a
// longer --for extends, --stop stops it at once and removes the pin; an
// unusable record.json makes --for refuse.
func TestRecordOp(t *testing.T) {
	t.Parallel()
	e, adb := installFake(t, "FAKEA")
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.5.0", exitAfter(time.Hour), adb)
	ctx := context.Background()
	if !s.Manager().Idle() || len(e.Calls()) != 0 {
		t.Fatal("not idle at start")
	}
	if _, err := s.record(ctx, RecordArgs{For: "30"}); err == nil || !strings.Contains(err.Error(), "--for") {
		t.Fatalf("unitless duration: %v", err)
	}
	if _, err := s.record(ctx, RecordArgs{For: "25h"}); err == nil || !strings.Contains(err.Error(), "24h") {
		t.Fatalf("25h: %v", err)
	}
	rep, err := s.record(ctx, RecordArgs{For: "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Action != "pinned" || rep.Deadline == nil || len(rep.Devices) != 1 || rep.Devices[0].ID != "FAKEA" || !rep.Devices[0].Recording || rep.Devices[0].Pin == nil {
		t.Fatalf("reply %+v", rep)
	}
	pf, _, _ := ReadPins(ctrl, time.Now())
	if p := pf.Pins["FAKEA"]; p == nil || p.Deadline.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("record.json %+v", pf)
	}
	if txt := s.Info().Text(); !strings.Contains(txt, "FAKEA: pinned until") || !strings.Contains(txt, "recording: 1 device(s): FAKEA") {
		t.Fatalf("status:\n%s", txt)
	}
	rep, err = s.record(ctx, RecordArgs{Stop: true})
	if err != nil || rep.Action != "stopped" {
		t.Fatalf("stop: %+v %v", rep, err)
	}
	if s.Manager().Wanted("FAKEA") || !s.Manager().Idle() {
		t.Fatal("still recording after --stop")
	}
	pf, _, _ = ReadPins(ctrl, time.Now())
	if len(pf.Pins) != 0 {
		t.Fatalf("pin kept: %+v", pf.Pins)
	}
	// unusable record.json: --for refuses, the file stays
	os.WriteFile(ctrl.PinsPath(), []byte(`{"v": 9}`), 0o600)
	s.loadPins(nil)
	if _, err := s.record(ctx, RecordArgs{For: "5m"}); err == nil || !strings.Contains(err.Error(), "format version 9") {
		t.Fatalf("unusable file: %v", err)
	}
	if b, _ := os.ReadFile(ctrl.PinsPath()); string(b) != `{"v": 9}` {
		t.Fatalf("overwritten: %s", b)
	}
}

// set-root keeps started devices, their timers and the pins.
func TestSetRootKeepsStartedDevices(t *testing.T) {
	t.Parallel()
	_, adb := installFake(t, "FAKEA")
	ctrl := testControl(t)
	s := startTest(t, ctrl, "0.5.0", exitAfter(time.Hour), adb)
	if _, err := s.record(context.Background(), RecordArgs{For: "10m"}); err != nil {
		t.Fatal(err)
	}
	last := s.Manager().LastCall("FAKEA")
	newRoot := filepath.Join(t.TempDir(), "r2")
	s.stopManager()
	s.restartIn(newRoot)
	m := s.Manager()
	if m.Base != newRoot || !m.Wanted("FAKEA") || !m.LastCall("FAKEA").Equal(last) || s.pinUntil("FAKEA").IsZero() {
		t.Fatalf("after set-root: base %s wanted %v last %v pin %v", m.Base, m.Wanted("FAKEA"), m.LastCall("FAKEA"), s.pinUntil("FAKEA"))
	}
}

// installFake installs a fake adb with online devices; the option points
// the service's device manager at it.
func installFake(t *testing.T, serials ...string) (fakeadb.Env, func(*Options)) {
	t.Helper()
	dir := t.TempDir()
	adb, err := fakeadb.Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := fakeadb.Env{Dir: dir}
	states := map[string]string{}
	for _, s := range serials {
		e.AddDevice(s, "boot-"+s)
		e.Log(s, fakeadb.Line(time.Now(), 100, "PRE", "pre"))
		states[s] = "device"
	}
	e.SetDevices(states)
	return e, func(o *Options) { o.ADBPath = adb }
}
