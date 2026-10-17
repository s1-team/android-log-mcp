package service

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

func TestStableExeOutsideDataRoot(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	out, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { run.Process.Kill(); run.Wait() }()
	// the copy is running (its image is mapped) once it printed "ready"
	ready := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(out).ReadString('\n'); ready <- l }()
	waitUntil(t, "the running copy to start", 60*time.Second, func() bool {
		select {
		case l := <-ready:
			if !strings.HasPrefix(l, "ready") {
				t.Fatalf("the running copy did not start: %q", l)
			}
			return true
		default:
			return false
		}
	})
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
	// Windows may release the image a moment after exit
	waitUntil(t, "the leftover of the old copy to be removed", 60*time.Second, func() bool {
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
		return len(left) == 0
	})
}
