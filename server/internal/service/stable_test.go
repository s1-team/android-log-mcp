package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

func TestStableExeOutsideDataRoot(t *testing.T) {
	c := DefaultControl()
	if got, want := c.StableExe(), filepath.Join(layout.DefaultDir(), "exe", exeName()); got != want {
		t.Fatalf("StableExe = %s, want %s", got, want)
	}
	if layout.OwnEntry("exe", true, true) || layout.OwnEntry("exe", true, false) {
		t.Fatal("exe/ must not be something set-root --delete-old removes")
	}
}

// installFile replaces a copy that is running (on Windows by renaming it
// aside), and the next install removes the renamed copy: bounded to one
// binary plus at most one leftover.
func TestInstallFileReplacesRunningCopy(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "exe", exeName())
	if _, up, err := installFile(os.Args[0], dst); err != nil || !up {
		t.Fatalf("first install: updated %v err %v", up, err)
	}
	if _, up, err := installFile(os.Args[0], dst); err != nil || up {
		t.Fatalf("same content: updated %v err %v", up, err)
	}
	run := exec.Command(dst, "-test.run=^$")
	run.Env = append(os.Environ(), "ALM_TEST_SLEEP=1")
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { run.Process.Kill(); run.Wait() }()
	time.Sleep(300 * time.Millisecond)
	src := filepath.Join(dir, "new-binary")
	os.WriteFile(src, []byte("a newer binary"), 0o755)
	if _, up, err := installFile(src, dst); err != nil || !up {
		t.Fatalf("replace running copy: updated %v err %v", up, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "a newer binary" {
		t.Fatalf("dst not replaced: %q", b)
	}
	run.Process.Kill()
	run.Wait()
	var left []string
	for i := 0; i < 50; i++ { // Windows may release the image a moment after exit
		if _, _, err := installFile(src, dst); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(filepath.Dir(dst))
		left = left[:0]
		for _, e := range ents {
			if e.Name() != exeName() {
				left = append(left, e.Name())
			}
		}
		if len(left) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("leftovers after the old copy stopped: %s", strings.Join(left, ", "))
}
