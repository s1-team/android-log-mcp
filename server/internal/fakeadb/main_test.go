package fakeadb

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// This test binary is copied as the fake adb by Install (the install tests).
func TestMain(m *testing.M) {
	if Is() {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// Install returns a copy that can be executed at once even while other
// goroutines fork (ETXTBSY, #103): the first exec after Install must work.
func TestInstallIsExecutableUnderForks(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1): ETXTBSY is a unix problem")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					exec.Command(truePath).Run()
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		adb, err := Install(dir)
		if err != nil {
			t.Fatalf("Install %d: %v", i, err)
		}
		cmd := exec.Command(adb, "version")
		cmd.Env = append(os.Environ(), EnvDir+"="+dir)
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), "Android Debug Bridge") {
			t.Fatalf("first exec %d: %v %q", i, err, out)
		}
	}
}
