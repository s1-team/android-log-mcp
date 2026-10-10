package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath makes p absolute and clean and resolves symlinks in its
// longest existing prefix (so /tmp vs /private/tmp, or a symlinked parent,
// compare correctly).
func ResolvePath(p string) (string, error) {
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

// Within reports whether p is inside dir (not equal).
func Within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// OwnEntry reports whether a root's top-level entry is android-log-mcp's
// own structure (what set-root --delete-old removes). run/ only counts
// when the root is not the control directory (which keeps run/ and
// config.json).
func OwnEntry(name string, dir, control bool) bool {
	if !dir {
		return false
	}
	switch {
	case versionDir.MatchString(name), name == "tmp", name == "bin", name == "log":
		return true
	case name == "run":
		return !control
	}
	return false
}

// ValidateTarget checks a set-root target against the current root and the
// control directory and returns it resolved. The target must be absolute,
// not the current root, not inside or a parent of it, not inside the
// control directory, writable, and either new/empty, the control directory
// (the default root) or already an android-log-mcp root. It creates the
// directory if needed (created reports that, for a rollback).
func ValidateTarget(target, current, control string) (resolved string, created bool, err error) {
	if strings.TrimSpace(target) == "" {
		return "", false, errors.New("empty directory")
	}
	if !filepath.IsAbs(target) {
		return "", false, fmt.Errorf("%s is not an absolute path", target)
	}
	t, err := ResolvePath(target)
	if err != nil {
		return "", false, err
	}
	cur, err := ResolvePath(current)
	if err != nil {
		return "", false, err
	}
	ctl, err := ResolvePath(control)
	if err != nil {
		return "", false, err
	}
	switch {
	case t == cur:
		return "", false, fmt.Errorf("%s is already the data root", t)
	case Within(t, cur):
		return "", false, fmt.Errorf("%s is inside the current root %s", t, cur)
	case Within(cur, t):
		return "", false, fmt.Errorf("%s is a parent of the current root %s", t, cur)
	case Within(t, ctl):
		return "", false, fmt.Errorf("%s is inside the control directory %s", t, ctl)
	}
	ents, err := os.ReadDir(t)
	switch {
	case err == nil && len(ents) > 0 && t != ctl:
		known, ours := false, true // has data of ours / has nothing else
		for _, e := range ents {
			known = known || (e.IsDir() && versionDir.MatchString(e.Name()))
			ours = ours && OwnEntry(e.Name(), e.IsDir(), false)
		}
		known = known || ours
		if !known {
			return "", false, fmt.Errorf("%s is not empty and is not an android-log-mcp data root; choose a new or empty directory", t)
		}
	case err != nil && os.IsNotExist(err):
		created = true
	case err != nil:
		return "", false, err
	}
	if err := os.MkdirAll(t, 0o755); err != nil {
		return "", false, fmt.Errorf("%s: cannot create: %v", t, err)
	}
	f, err := os.CreateTemp(t, ".write-test-*")
	if err != nil {
		if created {
			os.Remove(t)
		}
		return "", false, fmt.Errorf("%s is not writable: %v", t, err)
	}
	f.Close()
	os.Remove(f.Name())
	return t, created, nil
}

// RemoveOwn deletes android-log-mcp's own structure (see OwnEntry) from
// root, and root itself if nothing else is left (never the control
// directory). It returns what was removed and what remains of ours.
func RemoveOwn(root string, control bool) (removed, remained []string, rootRemoved bool, errs []string) {
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, true, nil
		}
		return nil, []string{root}, false, []string{err.Error()}
	}
	other := false
	for _, e := range ents {
		p := filepath.Join(root, e.Name())
		if !OwnEntry(e.Name(), e.IsDir(), control) {
			other = true
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err.Error())
			remained = append(remained, p)
			continue
		}
		removed = append(removed, p)
	}
	if !control && !other && len(remained) == 0 {
		if err := os.Remove(root); err == nil {
			rootRemoved = true
		} else {
			errs = append(errs, err.Error())
		}
	}
	return
}
