package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A fake adb for lifecycle tests: a copy of this test binary named adb[.exe]
// (ALM_FAKE_ADB_DIR holds its state). Like the real one, any command starts
// a "server" if none runs, as a plain child process (the worst case: same
// process group, no setsid), which must survive the recorder.

func isFakeADB() bool {
	b := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	return b == "adb" && os.Getenv("ALM_FAKE_ADB_DIR") != ""
}

func fakeADB(args []string) int {
	dir := os.Getenv("ALM_FAKE_ADB_DIR")
	if len(args) == 0 {
		return 1
	}
	switch args[0] {
	case "fake-server":
		os.WriteFile(filepath.Join(dir, "server.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(2 * time.Minute) // bounded: never leak forever
		return 0
	case "version":
		fmt.Println("Android Debug Bridge version 1.0.41")
		return 0
	case "start-server":
		ensureFakeServer(dir)
		return 0
	case "track-devices":
		ensureFakeServer(dir)
		os.WriteFile(filepath.Join(dir, "track.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		// like the real one: frames on every change; an orphan dies on the
		// first write to the closed pipe
		for i := 0; i < 1200; i++ {
			if _, err := os.Stdout.WriteString("0000"); err != nil {
				return 1
			}
			time.Sleep(100 * time.Millisecond)
		}
		return 0
	case "devices":
		fmt.Print("List of devices attached\n\n")
		return 0
	}
	return 0
}

func ensureFakeServer(dir string) {
	if b, err := os.ReadFile(filepath.Join(dir, "server.pid")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 && processAlive(pid) {
			return
		}
	}
	cmd := exec.Command(os.Args[0], "fake-server")
	cmd.Start()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(dir, "server.pid")); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
