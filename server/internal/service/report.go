package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/redact"
	"github.com/s1crab/android-log-mcp/server/internal/report"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// ReportArgs is the "report" op: report.Options without the functions.
// Out must be absolute (the service has its own working directory).
type ReportArgs struct {
	Serials  []string       `json:"serials,omitempty"`
	Since    time.Duration  `json:"since,omitempty"`
	From     string         `json:"from,omitempty"`
	To       string         `json:"to,omitempty"`
	Package  string         `json:"package,omitempty"`
	MaxBytes int64          `json:"max_bytes,omitempty"`
	Note     string         `json:"note,omitempty"`
	AmStart  string         `json:"am_start,omitempty"`
	Redact   redact.Options `json:"redact"`
	Out      string         `json:"out"`
	Args     []string       `json:"args,omitempty"`
	NoADB    bool           `json:"no_adb,omitempty"`
	// LogsOnly: no recorded device data, only the diagnostics (service log …).
	LogsOnly bool `json:"logs_only,omitempty"`
	// DevicesList: run `adb devices -l` for the bundle even when NoADB
	// (an offline report); set by the CLI, not sent to the service.
	DevicesList bool `json:"-"`
	// Control is the control dir whose service state the diagnostics
	// describe (zero: none).
	Control Control `json:"-"`

	warnings []string // set by the service (backfill notes)
}

// ReportReply is the "report" op's reply.
type ReportReply struct {
	Manifest *report.Manifest `json:"manifest"`
	Log      []string         `json:"log,omitempty"` // progress / warnings, in order
	Root     string           `json:"root"`
	Version  string           `json:"version"` // of the process that built it
	PID      int              `json:"pid"`     // the process that built it
}

func init() {
	extraOps["report"] = func(ctx context.Context, s *Service, h *Hello, body json.RawMessage) (any, error) {
		var a ReportArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		s.opMu.RLock()
		defer s.opMu.RUnlock()
		s.mu.Lock()
		m, root, t := s.m, s.root, s.tools
		s.mu.Unlock()
		var notes []string
		if m != nil && t != nil && !a.NoADB {
			notes = startForReport(ctx, m, t, a.Serials)
		}
		if m != nil {
			for _, d := range m.Devices() {
				if d.Store != nil {
					d.Store.Flush() // the report reads the files
				}
			}
		}
		var adb *device.ADB
		if m != nil {
			adb = m.ADB()
		}
		a.warnings = notes
		a.Control = s.opt.Control
		return BuildReport(ctx, root, s.opt.Version, a, adb)
	}
}

// startForReport starts the report's devices (--serial, or the single
// online one) like a log-tool call and waits for their catch-up (max
// device.CatchUpWait); the returned notes go into the manifest.
func startForReport(ctx context.Context, m *device.Manager, t *mcpserver.Tools, serials []string) []string {
	names := serials
	if len(names) == 0 {
		names = []string{""}
	}
	var notes []string
	var startedIDs []string
	for _, n := range names {
		id, _, err := t.Target(n)
		if err != nil {
			if n == "" {
				notes = append(notes, "no device started for this report: "+err.Error())
			} else {
				notes = append(notes, fmt.Sprintf("%s: not started: %v", n, err))
			}
			continue
		}
		if id == "" {
			continue // not connected: its stored logs only
		}
		if err := m.Want(id, true); err != nil {
			notes = append(notes, fmt.Sprintf("%s: not started: %v", id, err))
			continue
		}
		startedIDs = append(startedIDs, id)
	}
	deadline := time.Now().Add(device.CatchUpWait)
	for _, id := range startedIDs {
		// a device being stopped right now is re-attached first (issue #15)
		d, ok := m.WaitRecording(ctx, id, max(0, time.Until(deadline)))
		if d == nil {
			continue
		}
		p := device.StartingProgress()
		if ok {
			p = d.WaitCaughtUp(ctx, max(0, time.Until(deadline)))
		}
		if p.Pending {
			notes = append(notes, fmt.Sprintf("backfill incomplete: %s: %s when the report was packed (waited %s)", d.Serial, p.BackfillText(d.Loc()), device.CatchUpWait))
		}
	}
	return notes
}

// BuildReport builds the zip for a (in the service, or in a CLI that holds
// service.lock because no service runs). adb nil = find it.
func BuildReport(ctx context.Context, root, version string, a ReportArgs, adb *device.ADB) (*ReportReply, error) {
	if !filepath.IsAbs(a.Out) {
		return nil, fmt.Errorf("report: output path %q is not absolute", a.Out)
	}
	rep := &ReportReply{Root: root, Version: version, PID: os.Getpid()}
	var mu sync.Mutex
	logf := func(f string, v ...any) {
		mu.Lock()
		rep.Log = append(rep.Log, fmt.Sprintf(f, v...))
		mu.Unlock()
	}
	opt := report.Options{Base: root, Serials: a.Serials, Since: a.Since, From: a.From, To: a.To, Package: a.Package, MaxBytes: a.MaxBytes,
		Note: a.Note, AmStart: a.AmStart, Redact: a.Redact, Out: a.Out, Version: version, Args: a.Args, Logf: logf, Warnings: a.warnings, LogsOnly: a.LogsOnly}
	var listADB *device.ADB
	if !a.NoADB || a.DevicesList {
		if adb == nil {
			if p, err := device.FindADB(); err == nil {
				adb = &device.ADB{Path: p}
			} else {
				logf("adb not found (%v): live device info and DropBox are skipped", err)
			}
		}
		listADB = adb
		if adb != nil && !a.NoADB {
			if devs, err := adb.Output(ctx, 10*time.Second, "devices"); err == nil {
				opt.Online = map[string]bool{}
				for _, d := range device.ParseDevices(devs) {
					if d.State == "device" {
						opt.Online[d.Serial] = true
					}
				}
				opt.Shell = func(ctx context.Context, serial, cmd string) (string, error) {
					return adb.Shell(ctx, serial, 60*time.Second, cmd)
				}
			} else {
				logf("adb devices failed (%v): live device info and DropBox are skipped", err)
			}
		}
	}
	if a.AmStart != "" && opt.Shell == nil {
		return nil, errors.New("--am-start needs adb (and not --no-adb)")
	}
	opt.Extras = diagnostics(ctx, root, a.Control, listADB, logf)
	man, err := report.Build(ctx, opt)
	if err != nil {
		return nil, err
	}
	rep.Manifest = man
	return rep, nil
}

// RunOrLocal runs remote against the running service, or, when no service
// runs, local with the configured root while holding run/service.lock (so
// no service starts meanwhile). It never starts a service and never
// replaces one.
func RunOrLocal(ctx context.Context, ctrl Control, version, role string, remote func(*Conn) error, local func(root string) error) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := Dial(ctx, ctrl, version, role)
		if err == nil {
			defer conn.Close()
			h := conn.Hello
			if !ProtoCompatible(Proto, h.MinProto, h.Proto) {
				if CompareVersions(version, h.Version) > 0 {
					return fmt.Errorf("the running android-log-mcp service (pid %d, version %s, protocol %d) is older than this command (version %s, protocol %d) and cannot serve it; "+
						"stop it with `android-log-mcp service stop` (the next MCP client starts the new version) and retry", h.PID, h.Version, h.Proto, version, Proto)
				}
				return &ErrIncompatible{Client: HelloReply{Version: version, Proto: Proto}, Service: h}
			}
			return remote(conn)
		}
		var re *ipc.RemoteError
		if errors.As(err, &re) {
			return fmt.Errorf("android-log-mcp service: %s", re.Msg)
		}
		if err := ipc.PrepareDir(ctrl.Run()); err != nil {
			return fmt.Errorf("run directory: %w", err)
		}
		if lock, ok := store.TryLock(ctrl.LockPath()); ok {
			defer store.ReleaseLock(lock)
			root, err := ConfiguredRoot(ctrl)
			if err != nil {
				return err
			}
			return local(root)
		}
		if time.Now().After(deadline) {
			pid := 0
			if st := ctrl.ReadState(); st != nil {
				pid = st.PID
			}
			return fmt.Errorf("an android-log-mcp service (pid %d) holds %s but does not answer on %s (%v); try: android-log-mcp service stop --force", pid, ctrl.LockPath(), ctrl.Socket(), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Report builds a report through the service, starting it detached when
// none runs (issue #11 §9: report is explicit use; the service starts and
// backfills the selected devices first). It never runs the service in
// this process: a failed detached start is an error.
func Report(ctx context.Context, ctrl Control, version string, a ReportArgs) (*ReportReply, error) {
	cl := NewClient(ClientOptions{Control: ctrl, Version: version, Role: "report", NoEmbedded: true})
	defer cl.Close()
	var r ReportReply
	if err := cl.Do(ctx, "report", a, &r); err != nil {
		var re *ipc.RemoteError
		if errors.As(err, &re) {
			return nil, errors.New(re.Msg)
		}
		return nil, err
	}
	return &r, nil
}
