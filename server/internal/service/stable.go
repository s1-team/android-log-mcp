package service

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StableExe is where `android-log-mcp install` puts the binary so MCP
// clients can run it directly (no node / npx / cmd.exe in between):
// <control dir>/exe/android-log-mcp[.exe]. It is in the fixed control
// directory, not under the data root, so set-root never moves or deletes
// it. Bounded: one binary, plus renamed previous copies only while they are
// still running (Windows; removed by the next install after they stop).
func (c Control) StableExe() string { return filepath.Join(c.Dir, "exe", exeName()) }

// InstallStable copies the running executable to ctrl.StableExe().
func InstallStable(ctrl Control) (path string, updated bool, err error) {
	src, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	return installFile(src, ctrl.StableExe())
}

// installFile atomically replaces dst with a copy of src (unless they are
// the same file or have the same content). If dst cannot be replaced
// because it is running (Windows), it is renamed to .old-<n> first; those
// leftovers are removed on the next call once nothing runs them.
func installFile(src, dst string) (string, bool, error) {
	dir := filepath.Dir(dst)
	cleanLeftovers(dir, filepath.Base(dst))
	if sameFile(src, dst) {
		return dst, false, nil
	}
	if eq, err := sameContent(src, dst); err != nil {
		return "", false, err
	} else if eq {
		return dst, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".install-*")
	if err != nil {
		return "", false, err
	}
	_, cerr := io.Copy(tmp, in)
	if err := tmp.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil {
		cerr = os.Chmod(tmp.Name(), 0o755)
	}
	if cerr == nil {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			// Windows: a running executable cannot be replaced, but it can be renamed
			old := dst + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			if e2 := os.Rename(dst, old); e2 != nil {
				cerr = fmt.Errorf("%s is in use and cannot be replaced: %v", dst, err)
			} else if e3 := os.Rename(tmp.Name(), dst); e3 != nil {
				os.Rename(old, dst)
				cerr = e3
			}
		}
	}
	os.Remove(tmp.Name())
	if cerr != nil {
		return "", false, fmt.Errorf("install %s: %w", dst, cerr)
	}
	return dst, true, nil
}

// cleanLeftovers removes interrupted copies and previous binaries that are
// no longer running (Windows refuses to delete those that are).
func cleanLeftovers(dir, base string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".install-") || strings.HasPrefix(n, base+".old-") {
			os.Remove(filepath.Join(dir, n))
		}
	}
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func sameContent(a, b string) (bool, error) {
	fb, err := os.Stat(b)
	if os.IsNotExist(err) {
		return false, nil
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	if fb.Size() != fa.Size() {
		return false, nil
	}
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(ha, hb), nil
}

func fileHash(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
