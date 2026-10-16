package fakeadb

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A recorded pid line names its process; a pid that belongs to another
// process by now (reused) is not running (#103, Windows).
func TestPidLineIdentity(t *testing.T) {
	pid := os.Getpid()
	line := pidLine(pid)
	p, id, _ := strings.Cut(line, ":")
	if p != strconv.Itoa(pid) || !sameProcess(pid, id) {
		t.Fatalf("pidLine %q is not this process", line)
	}
	if id != "" && sameProcess(pid, id+"1") {
		t.Fatal("a different start time matched")
	}
	cmd := startSleeper(t)
	other := cmd.Process.Pid
	if startTime(other) == "" {
		t.Skip("no process start time on this OS")
	}
	e := Env{Dir: t.TempDir()}
	pids := filepath.Join(e.Dir, "pids")
	os.WriteFile(pids, []byte(strconv.Itoa(other)+":1\n"), 0o644) // stale start time
	if got := e.Running(); len(got) != 0 {
		t.Fatalf("a reused pid counted as running: %v", got)
	}
	os.WriteFile(pids, []byte(pidLine(other)+"\n"), 0o644)
	if got := e.Running(); len(got) != 1 {
		t.Fatalf("a live process not counted: %v", got)
	}
}
