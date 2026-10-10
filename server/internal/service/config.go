package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Config is <control dir>/config.json, written only by set-root (under
// run/config.lock, atomically). Without it the data root is the control
// directory itself (the default root).
type Config struct {
	Root string `json:"root,omitempty"`
	// Cleanup lists old roots whose deletion (--delete-old) left something
	// of ours behind; the service retries them when it starts.
	Cleanup []string `json:"cleanup,omitempty"`
}

// ReadConfig reads config.json; a missing file is an empty config.
func ReadConfig(ctrl Control) (Config, error) {
	var c Config
	b, err := os.ReadFile(ctrl.ConfigPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s is invalid (%v); fix it or delete it to use the default data root %s", ctrl.ConfigPath(), err, ctrl.Dir)
	}
	if c.Root != "" && !filepath.IsAbs(c.Root) {
		return c, fmt.Errorf("%s: root %q is not absolute; fix it or delete the file to use the default data root %s", ctrl.ConfigPath(), c.Root, ctrl.Dir)
	}
	return c, nil
}

// WriteConfig writes config.json atomically (0600).
func WriteConfig(ctrl Control, c Config) error {
	if c.Root != "" && sameDir(c.Root, ctrl.Dir) {
		c.Root = "" // the default
	}
	if err := os.MkdirAll(ctrl.Dir, 0o755); err != nil {
		return err
	}
	if c.Root == "" && len(c.Cleanup) == 0 {
		if err := os.Remove(ctrl.ConfigPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(ctrl.ConfigPath(), append(b, '\n'), 0o600)
}

// ConfiguredRoot is the data root: config.json's root, else the control
// directory (os.UserCacheDir()/android-log-mcp).
func ConfiguredRoot(ctrl Control) (string, error) {
	c, err := ReadConfig(ctrl)
	if err != nil {
		return "", err
	}
	if c.Root == "" {
		return ctrl.Dir, nil
	}
	return filepath.Clean(c.Root), nil
}

// ErrConfigBusy: another set-root holds run/config.lock.
var ErrConfigBusy = errors.New("another set-root is in progress (run/config.lock is held)")

// lockConfig takes run/config.lock, waiting up to timeout.
func lockConfig(ctrl Control, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		if f, ok := store.TryLock(ctrl.ConfigLockPath()); ok {
			return f, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w; waited %s for %s", ErrConfigBusy, timeout, ctrl.ConfigLockPath())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
