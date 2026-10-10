package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The data root is configured in one per-user file that every process
// reads (issue #9):
//
//	<os.UserConfigDir()>/android-log-mcp/config.json   {"root": "<dir>", "switch": {...}}
//
// Without the file (or without "root") the root is DefaultDir(). The file is
// written only by `android-log-mcp set-root`, atomically (tmp + rename).
// Running processes re-read it on every maintenance tick and move to a new
// root while they run (device.Manager.SyncRoot).

// RemovedHomeEnv is the environment variable that used to relocate the root.
const RemovedHomeEnv = "ANDROID_LOG_MCP_HOME"

// ErrHomeRemoved explains the removal of ANDROID_LOG_MCP_HOME / --home.
var ErrHomeRemoved = errors.New(RemovedHomeEnv + " and --home were removed in 0.4.0: the data root is now set once for all processes with `android-log-mcp set-root <dir> --keep-old|--delete-old` (stored in " + "<user config dir>/android-log-mcp/config.json); unset " + RemovedHomeEnv + " / drop --home from your MCP client configuration")

// Config is the content of config.json.
type Config struct {
	Root string `json:"root,omitempty"`
	// Switch is set while a set-root move is pending: running recorders
	// move to Root and acknowledge in their owner locks; set-root then
	// keeps or deletes From and clears the marker.
	Switch *Switch `json:"switch,omitempty"`
}

// Switch is a pending root change.
type Switch struct {
	From        string    `json:"from"`
	To          string    `json:"to"`
	Old         string    `json:"old"` // "keep" | "delete"
	RequestedAt time.Time `json:"requested_at"`
	PID         int       `json:"pid"`
}

// ConfigPath is <os.UserConfigDir()>/android-log-mcp/config.json.
func ConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(DefaultDir(), "config") // no config dir (unusual): next to the default data
	}
	return filepath.Join(d, "android-log-mcp", "config.json")
}

// ReadConfig reads config.json; a missing file is an empty config.
func ReadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %v", ConfigPath(), err)
	}
	if c.Root != "" && !filepath.IsAbs(c.Root) {
		return c, fmt.Errorf("%s: root %q is not absolute", ConfigPath(), c.Root)
	}
	return c, nil
}

// WriteConfig writes config.json atomically (tmp + rename).
func WriteConfig(c Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	b = append(b, '\n')
	tmp := fmt.Sprintf("%s.tmp%d", p, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ConfiguredRoot is the data root every process uses: config.json's root,
// else DefaultDir().
func ConfiguredRoot() (string, error) {
	c, err := ReadConfig()
	if err != nil {
		return "", err
	}
	if c.Root == "" {
		return DefaultDir(), nil
	}
	return filepath.Clean(c.Root), nil
}

// CheckRemovedHome returns ErrHomeRemoved when the removed env var is set.
func CheckRemovedHome() error {
	if os.Getenv(RemovedHomeEnv) != "" {
		return ErrHomeRemoved
	}
	return nil
}
