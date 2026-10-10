package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SetRootOptions configure SetRoot (`android-log-mcp set-root`).
type SetRootOptions struct {
	Target    string
	KeepOld   bool
	DeleteOld bool
	// Timeout for the running recorders' acknowledgement (default 150 s).
	Timeout time.Duration
	// Poll interval while waiting (default 1 s).
	Poll time.Duration
	// OwnersDir (default DefaultOwnersDir()).
	OwnersDir string
	// Logf reports progress (optional).
	Logf func(string, ...any)
}

// SetRootResult is the outcome (also printed as JSON with --json).
type SetRootResult struct {
	OldRoot string `json:"old_root"`
	NewRoot string `json:"new_root"`
	// Outcome: "switched" (done, old root handled), "timeout" (config
	// switched, but some recorder did not move yet; old root untouched).
	Outcome string `json:"outcome"`
	// Recorders that moved (pid → devices) / did not move in time.
	Moved   []string `json:"moved,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
	Waited  string   `json:"waited"`
	// Old root handling.
	Old            string   `json:"old"` // "keep" | "delete"
	Removed        []string `json:"removed,omitempty"`
	Remained       []string `json:"remained,omitempty"`
	OldRootRemoved bool     `json:"old_root_removed"`
	Errors         []string `json:"errors,omitempty"`
}

// ErrSwitchTimeout is returned when a recorder did not acknowledge in time.
var ErrSwitchTimeout = errors.New("timed out waiting for running recorders to move to the new root")

// resolvePath makes p absolute and clean and resolves symlinks in its
// longest existing prefix (so /tmp vs /private/tmp, or a symlinked parent,
// compare correctly).
func resolvePath(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	a = filepath.Clean(a)
	rest := ""
	cur := a
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return a, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether p is inside dir (not equal).
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ownEntry reports whether a root's top-level entry is android-log-mcp's own
// structure (deleted by --delete-old).
func ownEntry(name string, dir bool) bool {
	if !dir {
		return name == MarkerName
	}
	return versionDir.MatchString(name) || name == "tmp" || name == "run"
}

// ValidateTarget checks a set-root target against the current root and
// returns it resolved. The target must be absolute after resolving, not the
// current root, not inside it, not a parent of it, writable, and either
// new/empty, the default root, or already an android-log-mcp root.
func ValidateTarget(target, current string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", errors.New("empty directory")
	}
	t, err := resolvePath(target)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(t) {
		return "", fmt.Errorf("%s is not an absolute path", t)
	}
	cur, err := resolvePath(current)
	if err != nil {
		return "", err
	}
	switch {
	case t == cur:
		return "", fmt.Errorf("%s is already the data root", t)
	case within(t, cur):
		return "", fmt.Errorf("%s is inside the current root %s", t, cur)
	case within(cur, t):
		return "", fmt.Errorf("%s is a parent of the current root %s", t, cur)
	}
	if ents, err := os.ReadDir(t); err == nil && len(ents) > 0 {
		def, _ := resolvePath(DefaultDir())
		known := t == def
		for _, e := range ents {
			known = known || e.Name() == MarkerName || (e.IsDir() && versionDir.MatchString(e.Name()))
		}
		if !known {
			return "", fmt.Errorf("%s is not empty and is not an android-log-mcp data root (it would also hold tmp/ and run/ there); choose a new or empty directory", t)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(t, 0o755); err != nil {
		return "", fmt.Errorf("%s: cannot create: %v", t, err)
	}
	f, err := os.CreateTemp(t, ".write-test-*")
	if err != nil {
		return "", fmt.Errorf("%s is not writable: %v", t, err)
	}
	f.Close()
	os.Remove(f.Name())
	return t, nil
}

// pendingOwners lists the live owner locks that do not report root yet.
func pendingOwners(owners, root string) (moved, waiting []string) {
	r := Root{OwnersDir: owners}
	for id, o := range r.LiveOwners() {
		desc := fmt.Sprintf("%s (pid %d, version %s)", id, o.PID, nz(o.Version, "?"))
		got, _ := resolvePath(nz(o.Root, "?"))
		if o.Root != "" && got == root {
			moved = append(moved, desc)
		} else {
			waiting = append(waiting, desc+" still writes "+nz(o.DataDir, "?"))
		}
	}
	sort.Strings(moved)
	sort.Strings(waiting)
	return
}

func nz(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// SetRoot switches the configured data root (see config.go). It validates
// the target, writes config.json with a pending switch, waits until every
// running recorder reports the new root in its owner lock (they move on
// their next maintenance tick), then keeps or deletes the old root and
// clears the marker. With no recorder running the switch is immediate. On
// timeout the config stays switched (new processes use the new root,
// recorders move when they next check) and the old root is not touched;
// running the same command again finishes the switch.
func SetRoot(o SetRootOptions) (SetRootResult, error) {
	var res SetRootResult
	if o.KeepOld == o.DeleteOld {
		return res, errors.New("exactly one of --keep-old or --delete-old is required")
	}
	res.Old = "keep"
	if o.DeleteOld {
		res.Old = "delete"
	}
	if o.Timeout <= 0 {
		o.Timeout = 150 * time.Second
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.OwnersDir == "" {
		o.OwnersDir = DefaultOwnersDir()
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg, err := ReadConfig()
	if err != nil {
		return res, err
	}
	current := cfg.Root
	if current == "" {
		current = DefaultDir()
	}
	var target string
	if t, _ := resolvePath(o.Target); cfg.Switch != nil && cfg.Root != "" && t == filepath.Clean(cfg.Root) {
		// finish a switch that timed out earlier
		target, current = t, cfg.Switch.From
		logf("resuming the pending switch %s -> %s", current, target)
	} else {
		if target, err = ValidateTarget(o.Target, current); err != nil {
			return res, err
		}
		if c, err := resolvePath(current); err == nil {
			current = c
		}
	}
	res.OldRoot, res.NewRoot = current, target
	cfg.Root = target
	cfg.Switch = &Switch{From: current, To: target, Old: res.Old, RequestedAt: time.Now().UTC(), PID: os.Getpid()}
	if err := WriteConfig(cfg); err != nil {
		return res, err
	}
	// Ensure the target is a proper root before anyone moves in.
	(Root{Dir: target, OwnersDir: o.OwnersDir}).Ensure()
	start := time.Now()
	for {
		moved, waiting := pendingOwners(o.OwnersDir, target)
		res.Moved, res.Waiting = moved, waiting
		if len(waiting) == 0 {
			break
		}
		if time.Since(start) >= o.Timeout {
			res.Outcome = "timeout"
			res.Waited = time.Since(start).Round(time.Second).String()
			return res, ErrSwitchTimeout
		}
		time.Sleep(o.Poll)
	}
	res.Waited = time.Since(start).Round(time.Millisecond).String()
	if o.DeleteOld {
		removeOwn(current, &res)
	}
	cfg.Switch = nil
	if err := WriteConfig(cfg); err != nil {
		res.Errors = append(res.Errors, "clear pending marker: "+err.Error())
	}
	res.Outcome = "switched"
	return res, nil
}

// removeOwn deletes android-log-mcp's own structure (v<N>/, tmp/, run/, the
// marker) from an old root and the root itself if nothing else is left.
func removeOwn(root string, res *SetRootResult) {
	ents, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			res.Errors = append(res.Errors, err.Error())
		}
		res.OldRootRemoved = os.IsNotExist(err)
		return
	}
	for _, e := range ents {
		p := filepath.Join(root, e.Name())
		if !ownEntry(e.Name(), e.IsDir()) {
			res.Remained = append(res.Remained, p)
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			res.Errors = append(res.Errors, err.Error())
			res.Remained = append(res.Remained, p)
			continue
		}
		res.Removed = append(res.Removed, p)
	}
	if len(res.Remained) == 0 {
		if err := os.Remove(root); err == nil {
			res.OldRootRemoved = true
		} else {
			res.Errors = append(res.Errors, err.Error())
		}
	}
}
