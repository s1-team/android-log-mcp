package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Recording pins (issue #11 §8): `record --for` and
// logcat_status(record_for) keep a device recording until a deadline,
// independent of calls and clients. They are persisted in
// <control dir>/run/record.json so a crash, a version handoff or a machine
// restart does not silently end a recording the user asked for.

const (
	// PinsFormat is record.json's "v".
	PinsFormat = 1
	// MaxPin is the longest pin; deadlines read further ahead are clamped.
	MaxPin = 24 * time.Hour
	// MaxRestarts is the automatic restart budget per (service version, pin).
	MaxRestarts = 10
	// CrashLoopN crashes within CrashLoopWindow suspend a pin.
	CrashLoopN      = 3
	CrashLoopWindow = time.Minute
)

// Suspension reasons.
const (
	SuspendedCrashLoop   = "crash-loop"
	SuspendedCrashBudget = "crash-budget"
)

// PinFile is run/record.json.
type PinFile struct {
	V    int             `json:"v"`
	Pins map[string]*Pin `json:"pins"`
}

// Pin keeps one device recording until Deadline.
type Pin struct {
	Deadline      time.Time   `json:"deadline"`
	SetAt         time.Time   `json:"set_at"`
	SetBy         string      `json:"set_by"`
	Restarts      Restarts    `json:"restarts"`
	RecentCrashes []time.Time `json:"recent_crashes,omitempty"`
	Suspended     string      `json:"suspended,omitempty"`
	LastError     string      `json:"last_error,omitempty"`
}

// Restarts counts automatic restarts of the service for a pin, for one
// service version.
type Restarts struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
}

// Active: the pin keeps its device recording (deadline ahead, not
// suspended).
func (p *Pin) Active(now time.Time) bool {
	return p != nil && p.Suspended == "" && p.Deadline.After(now)
}

// PinsPath is run/record.json; it lives in the control directory, so
// set-root never moves it.
func (c Control) PinsPath() string     { return filepath.Join(c.Run(), "record.json") }
func (c Control) PinsLockPath() string { return filepath.Join(c.Run(), "record.lock") }

// ErrPinsUnusable: record.json has an unknown format version or cannot be
// parsed. It is ignored (no pins) and never overwritten.
type ErrPinsUnusable struct {
	Path string
	Why  string
}

func (e *ErrPinsUnusable) Error() string {
	return fmt.Sprintf("%s %s; it is ignored and never overwritten: remove or fix it to use recording pins", e.Path, e.Why)
}

// ReadPins reads record.json: a missing file is empty; deadlines more
// than MaxPin ahead are clamped and past ones removed (warnings say what
// changed); an unknown version or an unparsable file is an
// *ErrPinsUnusable.
func ReadPins(ctrl Control, now time.Time) (*PinFile, []string, error) {
	pf := &PinFile{V: PinsFormat, Pins: map[string]*Pin{}}
	b, err := os.ReadFile(ctrl.PinsPath())
	if errors.Is(err, os.ErrNotExist) {
		return pf, nil, nil
	}
	if err != nil {
		return nil, nil, &ErrPinsUnusable{Path: ctrl.PinsPath(), Why: "cannot be read (" + err.Error() + ")"}
	}
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, nil, &ErrPinsUnusable{Path: ctrl.PinsPath(), Why: "cannot be parsed (" + err.Error() + ")"}
	}
	if head.V != PinsFormat {
		return nil, nil, &ErrPinsUnusable{Path: ctrl.PinsPath(), Why: fmt.Sprintf("has format version %d (this version understands %d)", head.V, PinsFormat)}
	}
	if err := json.Unmarshal(b, pf); err != nil {
		return nil, nil, &ErrPinsUnusable{Path: ctrl.PinsPath(), Why: "cannot be parsed (" + err.Error() + ")"}
	}
	if pf.Pins == nil {
		pf.Pins = map[string]*Pin{}
	}
	var warn []string
	for id, p := range pf.Pins {
		switch {
		case p == nil || !p.Deadline.After(now):
			delete(pf.Pins, id)
		case p.Deadline.After(now.Add(MaxPin)):
			warn = append(warn, fmt.Sprintf("%s: pin for %s ended %s, more than 24h ahead (clock change?); clamped to %s", ctrl.PinsPath(), id,
				p.Deadline.Local().Format(time.DateTime), now.Add(MaxPin).Local().Format(time.DateTime)))
			p.Deadline = now.Add(MaxPin)
		}
	}
	sort.Strings(warn)
	return pf, warn, nil
}

// WritePins writes record.json atomically.
func WritePins(ctrl Control, pf *PinFile) error {
	pf.V = PinsFormat
	b, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ctrl.PinsPath(), append(b, '\n'), 0o600)
}

// UpdatePins reads record.json, applies fn and writes it back, under
// run/record.lock. An unusable file is never written.
func UpdatePins(ctrl Control, now time.Time, fn func(pf *PinFile) error) (*PinFile, error) {
	lock, err := lockFile(ctrl.PinsLockPath(), 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer store.ReleaseLock(lock)
	pf, warn, err := ReadPins(ctrl, now)
	for _, w := range warn {
		log.Print(w)
	}
	if err != nil {
		return nil, err
	}
	if err := fn(pf); err != nil {
		if errors.Is(err, errNoChange) {
			return pf, nil
		}
		return pf, err
	}
	return pf, WritePins(ctrl, pf)
}

// errNoChange from an UpdatePins fn: nothing to write.
var errNoChange = errors.New("no change")

func lockFile(path string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		if f, ok := store.TryLock(path); ok {
			return f, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another process for more than %s", path, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SetPin pins id for d at now: the deadline becomes max(current, now+d)
// (never shortens); a suspension and the restart counters are cleared.
func (pf *PinFile) SetPin(id string, d time.Duration, by, version string, now time.Time) *Pin {
	p := pf.Pins[id]
	if p == nil || p.Suspended != "" {
		p = &Pin{}
		if old := pf.Pins[id]; old != nil && old.Deadline.After(now) {
			p.Deadline = old.Deadline
		}
	}
	if dl := now.Add(d); dl.After(p.Deadline) {
		p.Deadline = dl
	}
	p.SetAt, p.SetBy = now, by
	p.Restarts = Restarts{Version: version}
	p.RecentCrashes, p.Suspended, p.LastError = nil, "", ""
	pf.Pins[id] = p
	return p
}

// takeover applies a new service's start to the pins: a different version
// gets a fresh restart budget; the same version keeps the count.
func (pf *PinFile) takeover(version string) bool {
	changed := false
	for _, p := range pf.Pins {
		if p.Restarts.Version != version {
			p.Restarts = Restarts{Version: version}
			changed = true
		}
	}
	return changed
}

// crashed applies a crash of the previous service (detected by this
// service's start, which is an automatic restart) to every active pin:
// CrashLoopN crashes within CrashLoopWindow suspend it (crash-loop); more
// than MaxRestarts restarts for this version suspend it (crash-budget).
func (pf *PinFile) crashed(version, lastErr string, now time.Time) []string {
	var notes []string
	for id, p := range pf.Pins {
		if !p.Active(now) {
			continue
		}
		if p.Restarts.Version != version {
			p.Restarts = Restarts{Version: version}
		}
		p.Restarts.Count++
		kept := p.RecentCrashes[:0]
		for _, t := range p.RecentCrashes {
			if now.Sub(t) < CrashLoopWindow {
				kept = append(kept, t)
			}
		}
		p.RecentCrashes = append(kept, now)
		p.LastError = lastErr
		switch {
		case len(p.RecentCrashes) >= CrashLoopN:
			p.Suspended = SuspendedCrashLoop
		case p.Restarts.Count > MaxRestarts:
			p.Suspended = SuspendedCrashBudget
		}
		if p.Suspended != "" {
			notes = append(notes, fmt.Sprintf("%s: pin suspended (%s): the service crashed %d times within %s / %d automatic restarts for version %s; last error: %s; a new `android-log-mcp record --for` clears it",
				id, p.Suspended, len(p.RecentCrashes), CrashLoopWindow, p.Restarts.Count-1, version, lastErr))
		} else {
			notes = append(notes, fmt.Sprintf("%s: pinned recording restarted after a service crash (%d of %d automatic restarts for version %s); last error: %s", id, p.Restarts.Count, MaxRestarts, version, lastErr))
		}
	}
	sort.Strings(notes)
	return notes
}

// HasActivePin reports whether record.json has an active (unsuspended)
// pin; clients use it to restart a crashed service at once.
func HasActivePin(ctrl Control, now time.Time) bool {
	pf, _, err := ReadPins(ctrl, now)
	if err != nil {
		return false
	}
	for _, p := range pf.Pins {
		if p.Active(now) {
			return true
		}
	}
	return false
}

// Describe renders a pin for status output (deadline in host time).
func (p *Pin) Describe(now time.Time) string {
	if p == nil {
		return ""
	}
	s := fmt.Sprintf("pinned until %s (in %s; set by %s at %s)", p.Deadline.Local().Format("2006-01-02 15:04:05"), p.Deadline.Sub(now).Round(time.Second),
		orDefault(p.SetBy, "?"), p.SetAt.Local().Format("15:04:05"))
	if p.Restarts.Count > 0 {
		s += fmt.Sprintf("; %d of %d automatic restarts used (version %s)", p.Restarts.Count, MaxRestarts, p.Restarts.Version)
	}
	if p.Suspended != "" {
		s = fmt.Sprintf("pin SUSPENDED: %s (no automatic restarts; a new `record --for` / record_for clears it); ", p.Suspended) + s
	}
	if p.LastError != "" {
		s += "; last error: " + p.LastError
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
