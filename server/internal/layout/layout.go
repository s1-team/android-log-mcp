// Package layout owns the data root of android-log-mcp:
//
//	<root>/                       os.UserCacheDir()/android-log-mcp, or ANDROID_LOG_MCP_HOME
//	  run/                        runtime state (sockets, pid files) of live processes
//	  tmp/                        scratch files (expire after an hour)
//	  v1/<device-id>/             device data, format 1 (segments, meta.json, aux files)
//
//	os.UserCacheDir()/android-log-mcp/owners/<device-id>.lock
//	                              global "who records this device" lock + JSON owner info
//
// The owners directory is fixed per user, independent of
// ANDROID_LOG_MCP_HOME, so a device is recorded by at most one process per
// user whatever root each process uses. Everything the tool manages under
// the root is bounded by the total size cap and the retention window
// (Sweep); anything else there (e.g. directories of versions before 0.4) is
// ignored: never read, never deleted.
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Root is the data root plus the owners directory.
type Root struct {
	Dir string
	// OwnersDir holds the owner locks; "" = <Dir>/owners. The recorder
	// always uses DefaultOwnersDir().
	OwnersDir string
}

// DefaultDir is os.UserCacheDir()/android-log-mcp (the temp dir if the
// cache dir is unknown).
func DefaultDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "android-log-mcp")
}

// DefaultOwnersDir is the fixed per-user owners directory
// (os.UserCacheDir()/android-log-mcp/owners), independent of
// ANDROID_LOG_MCP_HOME.
func DefaultOwnersDir() string { return filepath.Join(DefaultDir(), "owners") }

func (r Root) Owners() string {
	if r.OwnersDir != "" {
		return r.OwnersDir
	}
	return filepath.Join(r.Dir, "owners")
}
func (r Root) Run() string { return filepath.Join(r.Dir, "run") }
func (r Root) Tmp() string { return filepath.Join(r.Dir, "tmp") }

// DeviceDir is the format-1 data directory of device id under base (the
// root, or ANDROID_LOG_MCP_HOME).
func DeviceDir(base, id string) string {
	return filepath.Join(store.DataDir(base), store.SanitizeSerial(id))
}

// Ensure creates the managed sub-directories.
func (r Root) Ensure() error {
	for _, d := range []string{r.Owners(), r.Run(), r.Tmp(), store.DataDir(r.Dir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ---- ownership ----

// Owner describes the process recording a device (the owner lock's content).
type Owner struct {
	PID     int       `json:"pid"`
	Version string    `json:"version,omitempty"`
	DataDir string    `json:"data_dir"`       // where the device's logs are written
	Addr    string    `json:"addr,omitempty"` // adb serial used to reach it
	Home    string    `json:"home,omitempty"` // relocated data root (ANDROID_LOG_MCP_HOME / --home), if not the default
	BootID  string    `json:"boot_id,omitempty"`
	Since   time.Time `json:"since"`
}

// String is a short human description ("pid 123, version 0.4.0, data <dir>").
func (o Owner) String() string {
	s := fmt.Sprintf("pid %d", o.PID)
	if o.Version != "" {
		s += ", version " + o.Version
	}
	if o.Home != "" {
		s += ", relocated root " + o.Home // ANDROID_LOG_MCP_HOME / --home
	}
	if o.DataDir != "" {
		s += ", data " + o.DataDir
	}
	return s
}

// OwnerLock is a held owners/<id>.lock.
type OwnerLock struct {
	f    *os.File
	Path string
	Info Owner
}

func (r Root) ownerPath(id string) string {
	return filepath.Join(r.Owners(), store.SanitizeSerial(id)+".lock")
}

// ErrOwned is returned by AcquireOwner when another process holds the lock.
var ErrOwned = errors.New("device is recorded by another android-log-mcp process")

// AcquireOwner takes the global ownership lock of device id. When another
// process holds it, it returns ErrOwned and (best effort) that owner's info.
func (r Root) AcquireOwner(id string, info Owner) (*OwnerLock, *Owner, error) {
	if err := os.MkdirAll(r.Owners(), 0o755); err != nil {
		return nil, nil, err
	}
	p := r.ownerPath(id)
	for attempt := 0; attempt < 3; attempt++ {
		f, ok := store.TryLock(p)
		if !ok {
			o, _ := r.ReadOwner(id)
			return nil, o, ErrOwned
		}
		// the file may have been unlinked (sweep / a releasing owner)
		// between our open and the lock: only a lock on the inode that is
		// still at the path counts
		if runtime.GOOS != "windows" {
			fi, err1 := f.Stat()
			pi, err2 := os.Stat(p)
			if err1 != nil || err2 != nil || !os.SameFile(fi, pi) {
				f.Close()
				continue
			}
		}
		l := &OwnerLock{f: f, Path: p, Info: info}
		if err := l.write(); err != nil {
			l.Release()
			return nil, nil, err
		}
		return l, nil, nil
	}
	return nil, nil, fmt.Errorf("owner lock %s keeps changing", p)
}

func (l *OwnerLock) write() error {
	b, _ := json.Marshal(l.Info)
	b = append(b, '\n')
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(b, 0)
	return err
}

// Update rewrites the owner info (e.g. a new adb address).
func (l *OwnerLock) Update(fn func(o *Owner)) {
	if l == nil || l.f == nil {
		return
	}
	fn(&l.Info)
	l.write()
}

// Touch refreshes the lock's mtime; on platforms without flock a lock that
// is not refreshed for a minute is considered stale.
func (l *OwnerLock) Touch() {
	if l == nil || l.f == nil {
		return
	}
	now := time.Now()
	os.Chtimes(l.Path, now, now)
}

// Release gives up ownership and removes the lock file.
func (l *OwnerLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	if runtime.GOOS != "windows" {
		os.Remove(l.Path) // while still holding the lock (see AcquireOwner)
	}
	store.ReleaseLock(l.f) // removes the file on Windows
	l.f = nil
}

// ReadOwner returns the info of device id's owner and whether the lock is
// currently held.
func (r Root) ReadOwner(id string) (*Owner, bool) {
	p := r.ownerPath(id)
	held := store.LockHeld(p)
	b, err := os.ReadFile(p)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return nil, held
	}
	var o Owner
	if json.Unmarshal(b, &o) != nil {
		return nil, held
	}
	return &o, held
}

// OwnerHeld reports whether some process owns device id.
func (r Root) OwnerHeld(id string) bool { return store.LockHeld(r.ownerPath(id)) }

// LiveOwners returns the owner info of every device currently recorded by a
// live process (any root), keyed by the lock file's device ID.
func (r Root) LiveOwners() map[string]*Owner {
	out := map[string]*Owner{}
	ents, _ := os.ReadDir(r.Owners())
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".lock")
		if !ok || e.IsDir() {
			continue
		}
		if o, held := r.ReadOwner(id); held && o != nil {
			out[id] = o
		}
	}
	return out
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
