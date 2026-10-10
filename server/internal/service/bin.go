package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// BinDir is where service binaries are installed under the data root:
// <root>/bin/<version>-<content hash>/android-log-mcp[.exe]. The service
// runs from this copy, not from wherever the client came from (an npx
// cache that npm may clean or update, which Windows could not do while the
// service runs from it). It counts towards the total size cap; the sweep
// keeps the running version plus the newest other one.
func BinDir(root string) string { return filepath.Join(root, "bin") }

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func exeName() string {
	if runtime.GOOS == "windows" {
		return "android-log-mcp.exe"
	}
	return "android-log-mcp"
}

// InstallBinary copies the running executable into root's bin/ (once per
// version and content) and returns the copy's path.
func InstallBinary(root, version string) (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	name := unsafeChars.ReplaceAllString(version, "_") + "-" + hex.EncodeToString(h.Sum(nil))[:12]
	dir := filepath.Join(BinDir(root), name)
	dst := filepath.Join(dir, exeName())
	if fi, err := os.Stat(dst); err == nil && fi.Size() == size {
		now := time.Now()
		os.Chtimes(dst, now, now) // mtime = last started: the sweep keeps the newest
		return dst, nil
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".install-*")
	if err != nil {
		return "", err
	}
	_, cerr := io.Copy(tmp, f)
	if err := tmp.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil {
		cerr = os.Chmod(tmp.Name(), 0o755)
	}
	if cerr == nil {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			// another client installed it concurrently (Windows: in use)
			if fi, e2 := os.Stat(dst); e2 == nil && fi.Size() == size {
				cerr = nil
			} else {
				cerr = err
			}
		}
	}
	os.Remove(tmp.Name())
	if cerr != nil {
		return "", fmt.Errorf("install service binary into %s: %w", dir, cerr)
	}
	return dst, nil
}
