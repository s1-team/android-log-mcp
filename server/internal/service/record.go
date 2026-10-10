package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
)

// loadPins reads record.json at start. prev is the service.json left
// behind by the previous service: present means it did not shut down
// (crash, kill -9, power loss), which counts against every active pin.
func (s *Service) loadPins(prev *State) {
	now := time.Now()
	crashed := prev != nil && prev.PID != os.Getpid()
	var notes []string
	pf, err := UpdatePins(s.opt.Control, now, func(pf *PinFile) error {
		changed := pf.takeover(s.opt.Version)
		if crashed {
			n := pf.crashed(s.opt.Version, crashError(prev), now)
			changed = changed || len(n) > 0
			notes = n
		}
		if !changed {
			return errNoChange
		}
		return nil
	})
	var ue *ErrPinsUnusable
	if errors.As(err, &ue) {
		log.Printf("recording pins: %v", err)
		pf = nil
	} else if err != nil {
		log.Printf("recording pins: %v", err)
	}
	for _, n := range notes {
		log.Print(n)
	}
	s.pinMu.Lock()
	s.pins, s.pinsErr, s.pinNotes = pf, nil, notes
	if ue != nil {
		s.pinsErr = ue
	}
	s.pinMu.Unlock()
}

// crashError describes how the previous service ended: the Go runtime's
// fatal error from its crash log if it wrote one, else the bare fact.
func crashError(prev *State) string {
	msg := fmt.Sprintf("service pid %d (version %s) ended without shutting down", prev.PID, prev.Version)
	if prev.Root == "" {
		return msg
	}
	for _, name := range []string{"crash.log", "crash.log.1"} {
		p := filepath.Join(LogDir(prev.Root), name)
		fi, err := os.Stat(p)
		if err != nil || fi.Size() == 0 || fi.ModTime().Before(prev.Started) {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			ln := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(ln, "panic:") || strings.HasPrefix(ln, "fatal error:") {
				f.Close()
				return msg + ": " + ln
			}
		}
		f.Close()
	}
	return msg
}

// startPinned starts every active pin's device (no call: only the pin
// keeps it).
func (s *Service) startPinned(m *device.Manager) {
	now := time.Now()
	for _, id := range s.activePins(now) {
		if err := m.Want(id, false); err != nil {
			log.Printf("%s: pinned recording: %v", id, err)
		}
	}
}

func (s *Service) activePins(now time.Time) []string {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	var out []string
	if s.pins == nil {
		return nil
	}
	for id, p := range s.pins.Pins {
		if p.Active(now) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) anyActivePin(now time.Time) bool { return len(s.activePins(now)) > 0 }

// pinUntil is the device's active pin deadline (zero: none).
func (s *Service) pinUntil(id string) time.Time {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if s.pins == nil {
		return time.Time{}
	}
	if p := s.pins.Pins[id]; p.Active(time.Now()) {
		return p.Deadline
	}
	return time.Time{}
}

func (s *Service) pin(id string) *Pin {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if s.pins == nil {
		return nil
	}
	if p := s.pins.Pins[id]; p != nil {
		c := *p
		return &c
	}
	return nil
}

// setPin pins id (deadline = max(current, now + d)), persisting it.
func (s *Service) setPin(id string, d time.Duration, by string) (time.Time, error) {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if s.pinsErr != nil {
		return time.Time{}, fmt.Errorf("cannot pin: %v", s.pinsErr)
	}
	now := time.Now()
	var dl time.Time
	pf, err := UpdatePins(s.opt.Control, now, func(pf *PinFile) error {
		dl = pf.SetPin(id, d, by, s.opt.Version, now).Deadline
		return nil
	})
	if err != nil {
		var ue *ErrPinsUnusable
		if errors.As(err, &ue) {
			s.pins, s.pinsErr = nil, ue
		}
		return time.Time{}, fmt.Errorf("cannot pin: %v", err)
	}
	s.pins = pf
	return dl, nil
}

// dropPin removes id's pin (if any) from record.json.
func (s *Service) dropPin(id string) error {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if s.pinsErr != nil {
		return nil // never write an unusable file; there is no pin to drop
	}
	pf, err := UpdatePins(s.opt.Control, time.Now(), func(pf *PinFile) error {
		if _, ok := pf.Pins[id]; !ok {
			return errNoChange
		}
		delete(pf.Pins, id)
		return nil
	})
	if err != nil {
		return err
	}
	s.pins = pf
	return nil
}

// pinTexts describes every pin (with suspensions) and an unusable file.
func (s *Service) pinTexts() []string {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	var out []string
	if s.pinsErr != nil {
		out = append(out, "pins: "+s.pinsErr.Error())
	}
	if s.pins == nil {
		return out
	}
	now := time.Now()
	ids := make([]string, 0, len(s.pins.Pins))
	for id := range s.pins.Pins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := s.pins.Pins[id]
		if p.Deadline.After(now) {
			out = append(out, id+": "+p.Describe(now))
		}
	}
	return out
}

// recordingText summarizes the recorded devices.
func (s *Service) recordingText() (string, []string) {
	m := s.Manager()
	if m == nil {
		return "", nil
	}
	ids := m.WantedIDs()
	if len(ids) == 0 {
		return "idle (starts on the first log tool call)", nil
	}
	now := time.Now()
	var lines []string
	for _, id := range ids {
		lines = append(lines, id+": "+deviceState(m, id, now, s.pinUntil(id), true))
	}
	return fmt.Sprintf("%d device(s): %s", len(ids), strings.Join(ids, ", ")), lines
}

// deviceState describes a started device: its state (unless !withState),
// backfill and when it stops (pinUntil: its active pin's deadline, or zero).
func deviceState(m *device.Manager, id string, now, pinUntil time.Time, withState bool) string {
	d := m.Get(id)
	var parts []string
	state := func(st string) {
		if withState {
			parts = append(parts, st)
		}
	}
	switch {
	case d == nil:
		parts = append(parts, "waiting for the device")
	case d.Status().Recording:
		state("recording")
		if p := d.CatchUp(); p.Pending {
			parts = append(parts, p.BackfillText(d.Loc()))
		} else {
			parts = append(parts, "caught up")
		}
	case d.Status().State == "device":
		state("starting")
	default:
		parts = append(parts, "not connected (resumes when it reconnects)")
	}
	switch idle := m.IdleIn(id, now); {
	case pinUntil.After(now):
		parts = append(parts, "kept by its pin")
	case idle < 0:
		parts = append(parts, "no idle stop")
	case m.LastCall(id).IsZero():
		parts = append(parts, "kept by its pin")
	default:
		parts = append(parts, fmt.Sprintf("idle stop in %s unless called again", idle.Round(time.Second)))
	}
	return strings.Join(parts, "; ")
}

// pinner implements mcpserver.Pins for the tools.
type pinner struct{ s *Service }

func (p pinner) Set(id string, d time.Duration, by string) (time.Time, error) {
	dl, err := p.s.setPin(id, d, by)
	if err != nil {
		return dl, err
	}
	if m := p.s.Manager(); m != nil {
		if err := m.Want(id, true); err != nil { // pinning counts as a call
			return dl, err
		}
	}
	return dl, nil
}

func (p pinner) Stop(id string) error {
	if err := p.s.dropPin(id); err != nil {
		return err
	}
	if m := p.s.Manager(); m != nil {
		m.Unwant(id, "stopped on request")
	}
	return nil
}

func (p pinner) Until(id string) time.Time { return p.s.pinUntil(id) }

func (p pinner) Describe(id string, loc *time.Location) string {
	pin := p.s.pin(id)
	if pin == nil || !pin.Deadline.After(time.Now()) {
		return ""
	}
	return pin.Describe(time.Now())
}

// RecordArgs is the "record" op (protocol 2): pin (For), stop (Stop) or,
// with neither, report the state.
type RecordArgs struct {
	Devices []string `json:"devices,omitempty"`
	For     string   `json:"for,omitempty"`
	Stop    bool     `json:"stop,omitempty"`
	By      string   `json:"by,omitempty"`
}

// RecordDevice is one device in a RecordReply.
type RecordDevice struct {
	ID        string     `json:"id"`
	State     string     `json:"state"` // recording / idle / ...
	Detail    string     `json:"detail,omitempty"`
	Recording bool       `json:"recording"`
	CaughtUp  bool       `json:"caught_up"`
	Lines     uint64     `json:"backfill_lines"`
	Pin       *Pin       `json:"pin,omitempty"`
	IdleStop  *time.Time `json:"idle_stop,omitempty"`
}

// RecordReply is the "record" op's reply.
type RecordReply struct {
	Action   string         `json:"action"` // pinned / stopped / state
	Devices  []RecordDevice `json:"devices"`
	Pins     []string       `json:"pins,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
	Service  int            `json:"service_pid"`
	Version  string         `json:"service_version"`
	Deadline *time.Time     `json:"deadline,omitempty"`
}

func init() {
	extraOps["record"] = func(ctx context.Context, s *Service, h *Hello, body json.RawMessage) (any, error) {
		var a RecordArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		return s.record(ctx, a)
	}
}

func (s *Service) record(ctx context.Context, a RecordArgs) (*RecordReply, error) {
	s.mu.Lock()
	m, t := s.m, s.tools
	s.mu.Unlock()
	rep := &RecordReply{Action: "state", Service: os.Getpid(), Version: s.opt.Version}
	by := a.By
	if by == "" {
		by = "android-log-mcp record"
	}
	var ids []string
	switch {
	case a.For != "":
		d, err := mcpserver.ParsePin(a.For)
		if err != nil {
			return nil, errors.New(strings.Replace(err.Error(), "record_for", "--for", 1))
		}
		if d == 0 {
			return nil, errors.New("--for 0: use `android-log-mcp record --stop` to stop recording")
		}
		s.pinMu.Lock()
		perr := s.pinsErr
		s.pinMu.Unlock()
		if perr != nil {
			return nil, fmt.Errorf("refusing to pin: %v", perr)
		}
		names := a.Devices
		if len(names) == 0 {
			names = []string{""}
		}
		for _, n := range names {
			id, sd, err := t.Target(n)
			if err != nil {
				return nil, err
			}
			if id == "" {
				return nil, fmt.Errorf("device %s is not connected: connect it first (recording needs the device online)", sd.Serial)
			}
			ids = append(ids, id)
		}
		var dl time.Time
		for _, id := range ids {
			if err := m.Want(id, true); err != nil {
				return nil, err
			}
			until, err := s.setPin(id, d, by)
			if err != nil {
				return nil, err
			}
			dl = until
		}
		rep.Action, rep.Deadline = "pinned", &dl
		// return once logcat runs (the backfill is not waited for)
		deadline := time.Now().Add(10 * time.Second)
		for _, id := range ids {
			for time.Now().Before(deadline) && ctx.Err() == nil {
				if d := m.Get(id); d != nil && d.Status().Recording {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		if mx := s.opt.Cfg.MaxAge; mx > 0 && d > mx {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the pin (%s) is longer than the retention (ANDROID_LOG_MCP_MAX_AGE=%s): the oldest lines are pruned while it runs", d, mx))
		}
	case a.Stop:
		names := a.Devices
		if len(names) == 0 {
			cands := map[string]bool{}
			for _, id := range m.WantedIDs() {
				cands[id] = true
			}
			for _, id := range s.activePins(time.Now()) {
				cands[id] = true
			}
			switch len(cands) {
			case 0:
				rep.Action = "stopped"
				rep.Notes = append(rep.Notes, "nothing is recorded")
			case 1:
				for id := range cands {
					names = []string{id}
				}
			default:
				var l []string
				for id := range cands {
					l = append(l, id)
				}
				sort.Strings(l)
				return nil, fmt.Errorf("%d devices are recorded; pass --device one of: %s", len(l), strings.Join(l, ", "))
			}
		}
		for _, n := range names {
			id := n
			if d := m.Get(n); d != nil {
				id = d.Serial
			}
			if err := (pinner{s}).Stop(id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
			rep.Notes = append(rep.Notes, id+": recording stopped and pin removed")
		}
		rep.Action = "stopped"
	}
	rep.Pins = s.pinTexts()
	s.pinMu.Lock()
	rep.Notes = append(rep.Notes, s.pinNotes...)
	s.pinMu.Unlock()
	seen := map[string]bool{}
	all := append(append([]string{}, ids...), m.WantedIDs()...)
	all = append(all, s.activePins(time.Now())...)
	now := time.Now()
	for _, id := range all {
		if seen[id] {
			continue
		}
		seen[id] = true
		rd := RecordDevice{ID: id, State: "idle", Pin: s.pin(id)}
		if rd.Pin != nil && !rd.Pin.Deadline.After(now) {
			rd.Pin = nil
		}
		if m.Wanted(id) {
			rd.State = "started"
			pinned := s.pinUntil(id)
			rd.Detail = deviceState(m, id, now, pinned, false)
			if d := m.Get(id); d != nil {
				st := d.Status()
				p := d.CatchUp()
				rd.Recording, rd.CaughtUp, rd.Lines = st.Recording, st.Recording && !p.Pending, p.Lines
				if st.Recording {
					rd.State = "recording"
				}
			}
			if idle := m.IdleIn(id, now); idle >= 0 && !m.LastCall(id).IsZero() && !pinned.After(now) {
				t := now.Add(idle)
				rd.IdleStop = &t
			}
		}
		rep.Devices = append(rep.Devices, rd)
	}
	return rep, nil
}
