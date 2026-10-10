package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// SetRootArgs is the "set-root" op.
type SetRootArgs struct {
	Target    string        `json:"target"`
	DeleteOld bool          `json:"delete_old"`
	Timeout   time.Duration `json:"timeout"` // waiting for config.lock and in-flight calls
}

// SetRootResult is its outcome (`set-root --json`).
type SetRootResult struct {
	OldRoot        string   `json:"old_root"`
	NewRoot        string   `json:"new_root"`
	Old            string   `json:"old"` // keep | delete
	Devices        []string `json:"devices,omitempty"`
	ServicePID     int      `json:"service_pid,omitempty"` // 0: no service ran; switched by the CLI
	Removed        []string `json:"removed,omitempty"`
	Remained       []string `json:"remained,omitempty"`
	OldRootRemoved bool     `json:"old_root_removed,omitempty"`
	Errors         []string `json:"errors,omitempty"`
	Took           string   `json:"took"`
}

// TestFailSetRoot injects a failure before the commit ("carry" or
// "commit"); tests only.
var TestFailSetRoot string

const defaultSetRootTimeout = 60 * time.Second

func init() {
	extraOps["set-root"] = func(ctx context.Context, s *Service, h *Hello, body json.RawMessage) (any, error) {
		var a SetRootArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		if a.Timeout <= 0 {
			a.Timeout = defaultSetRootTimeout
		}
		if !lockTimeout(&s.opMu, a.Timeout) {
			return nil, fmt.Errorf("set-root: tool calls still running after %s; retry", a.Timeout)
		}
		defer s.opMu.Unlock()
		log.Printf("set-root requested by pid %d: %s (%s)", h.PID, a.Target, map[bool]string{true: "--delete-old", false: "--keep-old"}[a.DeleteOld])
		res, err := switchRoot(s.opt.Control, s.Root(), a, s.stopManager, s.restartIn)
		if err != nil {
			log.Printf("set-root failed: %v", err)
			return nil, err
		}
		res.ServicePID = os.Getpid()
		return res, nil
	}
}

// restartIn starts recording in root (after set-root or its rollback).
func (s *Service) restartIn(root string) {
	if s.opt.Log != nil {
		if err := s.opt.Log.SetRoot(root); err != nil {
			log.Printf("service log in %s: %v", root, err)
		}
	}
	if s.opt.OnRootChange != nil {
		s.opt.OnRootChange(root)
	}
	s.startManager(root)
	if err := s.writeState(); err != nil {
		log.Printf("service: writing %s: %v", s.opt.Control.StatePath(), err)
	}
	log.Printf("recording in %s", root)
}

// lockTimeout write-locks mu, giving up after d.
func lockTimeout(mu interface {
	Lock()
	Unlock()
}, d time.Duration) bool {
	got := make(chan struct{})
	go func() { mu.Lock(); close(got) }()
	select {
	case <-got:
		return true
	case <-time.After(d):
		go func() { <-got; mu.Unlock() }()
		return false
	}
}

// switchRoot moves the data root from current to a.Target under
// run/config.lock. stop stops recording (nil: nothing records) and start
// resumes it in a root (after the switch, or in current on rollback).
// Everything before the config.json commit is rolled back on failure; the
// old-root handling after it is reported (Remained) and retried by the
// service at its next start.
func switchRoot(ctrl Control, current string, a SetRootArgs, stop func(), start func(string)) (*SetRootResult, error) {
	t0 := time.Now()
	res := &SetRootResult{OldRoot: current, Old: "keep"}
	if a.DeleteOld {
		res.Old = "delete"
	}
	if a.Timeout <= 0 {
		a.Timeout = defaultSetRootTimeout
	}
	if err := ipc.PrepareDir(ctrl.Run()); err != nil {
		return nil, fmt.Errorf("run directory: %w", err)
	}
	lock, err := lockConfig(ctrl, a.Timeout)
	if err != nil {
		return nil, err
	}
	defer store.ReleaseLock(lock)
	cfg, err := ReadConfig(ctrl)
	if err != nil {
		return nil, err
	}
	target, created, err := layout.ValidateTarget(a.Target, current, ctrl.Dir)
	if err != nil {
		return nil, err
	}
	res.NewRoot = target
	r := layout.Root{Dir: target}
	var made []string // managed dirs this switch created in an existing target
	for _, d := range []string{r.Tmp(), store.DataDir(target)} {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			made = append(made, d)
		}
	}
	if stop != nil {
		stop()
	}
	var carried []device.Carried
	rollback := func(cause error) (*SetRootResult, error) {
		device.Uncarry(carried)
		if created {
			os.RemoveAll(target)
		} else {
			for _, d := range made {
				os.Remove(d) // only if still empty
			}
		}
		if start != nil {
			start(current)
		}
		return nil, fmt.Errorf("%v; rolled back: the data root is still %s", cause, current)
	}
	if err := r.Ensure(); err != nil {
		return rollback(fmt.Errorf("prepare %s: %v", target, err))
	}
	carried, err = device.CarryOver(current, target)
	if err == nil && TestFailSetRoot == "carry" {
		err = errors.New("injected failure (carry)")
	}
	if err != nil {
		return rollback(fmt.Errorf("carry the recording state over: %v", err))
	}
	for _, c := range carried {
		res.Devices = append(res.Devices, c.ID)
	}
	cfg.Root = target
	kept := cfg.Cleanup[:0]
	for _, p := range cfg.Cleanup {
		if rp, _ := layout.ResolvePath(p); rp != target {
			kept = append(kept, p) // moving back into a root pending cleanup cancels it
		}
	}
	cfg.Cleanup = kept
	err = nil
	if TestFailSetRoot == "commit" {
		err = errors.New("injected failure (commit)")
	} else {
		err = WriteConfig(ctrl, cfg)
	}
	if err != nil {
		return rollback(fmt.Errorf("write %s: %v", ctrl.ConfigPath(), err))
	}
	// committed
	log.Printf("data root switched: %s -> %s (%d devices continue there)", current, target, len(carried))
	if start != nil {
		start(target)
	}
	if a.DeleteOld {
		res.Removed, res.Remained, res.OldRootRemoved, res.Errors = layout.RemoveOwn(current, sameDir(current, ctrl.Dir))
		if len(res.Remained) > 0 {
			cfg.Cleanup = append(cfg.Cleanup, current)
			if err := WriteConfig(ctrl, cfg); err != nil {
				res.Errors = append(res.Errors, "note the leftovers in config.json: "+err.Error())
			}
		}
	}
	res.Took = time.Since(t0).Round(time.Millisecond).String()
	return res, nil
}

func sameDir(a, b string) bool {
	ra, _ := layout.ResolvePath(a)
	rb, _ := layout.ResolvePath(b)
	return ra == rb
}

// retryCleanup retries the deletion of old roots noted in config.json
// (--delete-old leftovers, e.g. files that were open on Windows).
func retryCleanup(ctrl Control, root string) {
	lock, ok := store.TryLock(ctrl.ConfigLockPath())
	if !ok {
		return // a set-root runs; it handles the list
	}
	defer store.ReleaseLock(lock)
	cfg, err := ReadConfig(ctrl)
	if err != nil || len(cfg.Cleanup) == 0 {
		return
	}
	var kept []string
	for _, p := range cfg.Cleanup {
		if sameDir(p, root) || layout.Within(root, p) || layout.Within(p, root) {
			continue // the current root (moved back): nothing to delete
		}
		_, remained, _, errs := layout.RemoveOwn(p, sameDir(p, ctrl.Dir))
		if len(remained) > 0 {
			log.Printf("old data root %s: still cannot remove %v: %v", p, remained, errs)
			kept = append(kept, p)
		} else {
			log.Printf("old data root %s: leftovers removed", p)
		}
	}
	cfg.Cleanup = kept
	if err := WriteConfig(ctrl, cfg); err != nil {
		log.Printf("config: %v", err)
	}
}

// SetRoot switches the data root: through the running service, or in this
// process (holding run/service.lock) when none runs. It never starts a
// service.
func SetRoot(ctx context.Context, ctrl Control, version string, a SetRootArgs) (*SetRootResult, error) {
	if !filepath.IsAbs(a.Target) {
		return nil, fmt.Errorf("%s is not an absolute path", a.Target)
	}
	var res *SetRootResult
	err := RunOrLocal(ctx, ctrl, version, "set-root",
		func(c *Conn) error {
			var r SetRootResult
			if err := c.Do(ctx, "set-root", a, &r); err != nil {
				var re *ipc.RemoteError
				if errors.As(err, &re) {
					return errors.New(re.Msg)
				}
				return err
			}
			res = &r
			return nil
		},
		func(root string) error {
			r, err := switchRoot(ctrl, root, a, nil, nil)
			res = r
			return err
		})
	return res, err
}
