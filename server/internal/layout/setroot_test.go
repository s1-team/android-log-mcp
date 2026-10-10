package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTarget(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	cur, ctl := filepath.Join(tmp, "cur"), filepath.Join(tmp, "ctl")
	os.MkdirAll(cur, 0o755)
	os.MkdirAll(filepath.Join(ctl, "run"), 0o755)
	used := filepath.Join(tmp, "used")
	os.MkdirAll(filepath.Join(used, "v1", "dev"), 0o755)
	junk := filepath.Join(tmp, "junk")
	os.MkdirAll(junk, 0o755)
	os.WriteFile(filepath.Join(junk, "notes.txt"), nil, 0o644)
	ours := filepath.Join(tmp, "ours")
	os.MkdirAll(filepath.Join(ours, "log"), 0o755)
	for _, c := range []struct {
		target, want string
		created      bool
	}{
		{filepath.Join(tmp, "new"), "", true},
		{used, "", false},
		{ours, "", false},
		{ctl, "", false}, // the default root
		{"relative", "not an absolute path", false},
		{cur, "already the data root", false},
		{filepath.Join(cur, "x"), "inside the current root", false},
		{tmp, "parent of the current root", false},
		{filepath.Join(ctl, "x"), "inside the control directory", false},
		{junk, "not empty", false},
	} {
		got, created, err := ValidateTarget(c.target, cur, ctl)
		if c.want == "" {
			if err != nil || got != c.target || created != c.created {
				t.Errorf("%s: %q %v %v", c.target, got, created, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.target, err, c.want)
		}
	}
}

func TestRemoveOwn(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"v1/dev", "tmp", "bin/0.4.0-x", "log", "run", "mine"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	// control root: run/ and foreign entries stay, root stays
	removed, remained, rootRemoved, errs := RemoveOwn(root, true)
	if len(removed) != 4 || len(remained) != 0 || rootRemoved || len(errs) != 0 {
		t.Fatalf("control: %v %v %v %v", removed, remained, rootRemoved, errs)
	}
	for _, d := range []string{"run", "mine"} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			t.Fatalf("%s removed", d)
		}
	}
	// other root: run/ goes; the root only once nothing foreign is left
	_, _, rootRemoved, _ = RemoveOwn(root, false)
	if rootRemoved {
		t.Fatal("root with foreign entries removed")
	}
	os.Remove(filepath.Join(root, "mine"))
	os.MkdirAll(filepath.Join(root, "v2"), 0o755)
	if _, _, rootRemoved, errs = RemoveOwn(root, false); !rootRemoved {
		t.Fatalf("root not removed: %v", errs)
	}
}
