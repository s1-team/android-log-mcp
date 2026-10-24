package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Condition waits for the lifecycle tests (#226 PR 1, the #220/#222 lesson):
// every stage has its own generous budget (scaled by
// ALM_TEST_TIMEOUT_FACTOR, default 1, 2 on Windows and macOS runners), and a
// timeout dumps the service files, the service log tail, the registered
// fake-adb state and a goroutine dump, so one log diagnoses a flake.

func timeoutFactor() float64 {
	if s := os.Getenv("ALM_TEST_TIMEOUT_FACTOR"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return f
		}
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return 2
	}
	return 1
}

func scaled(d time.Duration) time.Duration { return time.Duration(float64(d) * timeoutFactor()) }

// windowsRaceHeavy limits concurrent service-spawning isoEnv/lazyEnv tests on
// Windows under -race. Full parallelism starves process starts and cleanup
// (#251 nightly 50m at -count=20); full serialization sums past the budget
// too. Cap 2 keeps overlap without the free-for-all. Other platforms are
// unlimited. Only tests that start a service (or fake-adb recording) should
// call heavyParallel; light isoEnv CLI checks use t.Parallel so they do not
// queue on this semaphore and waste testing.Parallel slots (#267).
var windowsRaceHeavy = make(chan struct{}, 2)

// heavyParallel marks t parallel, and on Windows under -race also takes a
// windowsRaceHeavy slot for the test body + cleanup.
func heavyParallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	if runtime.GOOS != "windows" || !raceEnabled {
		return
	}
	windowsRaceHeavy <- struct{}{}
	t.Cleanup(func() { <-windowsRaceHeavy })
}

var testDiags sync.Map // *testing.T -> []func() string

func diagFor(t *testing.T, f func() string) {
	t.Helper()
	var l []func() string
	if v, ok := testDiags.Load(t); ok {
		l = v.([]func() string)
	}
	testDiags.Store(t, append(l, f))
	t.Cleanup(func() { testDiags.Delete(t) })
}

// envDiag describes the service state of an isoEnv for a failure message.
func envDiag(env []string) string {
	dir := controlDirOf(env)
	var b strings.Builder
	if ents, err := os.ReadDir(filepath.Join(dir, "run")); err == nil {
		for _, e := range ents {
			fmt.Fprintf(&b, "run/%s\n", e.Name())
		}
	} else {
		fmt.Fprintf(&b, "run/: %v\n", err)
	}
	for _, name := range []string{filepath.Join("run", "service.json"), filepath.Join("run", "wanted.json"), filepath.Join("run", "record.json")} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			fmt.Fprintf(&b, "--- %s\n%s\n", name, data)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "log", "service.log")); err == nil {
		if len(data) > 4000 {
			data = data[len(data)-4000:]
		}
		fmt.Fprintf(&b, "--- service.log tail\n%s\n", data)
	}
	return b.String()
}

func dumpDiags(t *testing.T) string {
	var b strings.Builder
	if v, ok := testDiags.Load(t); ok {
		for _, f := range v.([]func() string) {
			b.WriteString(f())
			b.WriteString("\n")
		}
	}
	buf := make([]byte, 1<<16)
	buf = buf[:runtime.Stack(buf, true)]
	b.WriteString("--- goroutines\n")
	b.Write(buf)
	return b.String()
}

// waitUntil polls cond until it holds or scaled(timeout) passes.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	budget := scaled(timeout)
	deadline := time.Now().Add(budget)
	for delay := 10 * time.Millisecond; !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s\n%s", budget, what, dumpDiags(t))
		}
		time.Sleep(delay)
		if delay < 50*time.Millisecond {
			delay += 10 * time.Millisecond
		}
	}
}
