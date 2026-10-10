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
		m, root := s.m, s.root
		s.mu.Unlock()
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
		return BuildReport(ctx, root, s.opt.Version, a, adb)
	}
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
		Note: a.Note, AmStart: a.AmStart, Redact: a.Redact, Out: a.Out, Version: version, Args: a.Args, Logf: logf}
	if !a.NoADB {
		if adb == nil {
			if p, err := device.FindADB(); err == nil {
				adb = &device.ADB{Path: p}
			} else {
				logf("adb not found (%v): live device info and DropBox are skipped", err)
			}
		}
		if adb != nil {
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
			return fmt.Errorf("an android-log-mcp service (pid %d) holds %s but does not answer on %s (%v)", pid, ctrl.LockPath(), ctrl.Socket(), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Report builds a report through the running service, or in this process
// when none runs (see RunOrLocal).
func Report(ctx context.Context, ctrl Control, version string, a ReportArgs) (*ReportReply, error) {
	var rep *ReportReply
	err := RunOrLocal(ctx, ctrl, version, "report",
		func(c *Conn) error {
			var r ReportReply
			if err := c.Do(ctx, "report", a, &r); err != nil {
				var re *ipc.RemoteError
				if errors.As(err, &re) {
					return errors.New(re.Msg)
				}
				return err
			}
			rep = &r
			return nil
		},
		func(root string) error {
			r, err := BuildReport(ctx, root, version, a, nil)
			rep = r
			return err
		})
	return rep, err
}
