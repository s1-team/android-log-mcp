package service

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

// Condition waits for lifecycle tests (#226 PR 1, the #220/#222 lesson).
//
//   - waitUntil polls a condition with its own, generous budget per stage
//     (never one deadline shared by several stages); the budget is scaled by
//     ALM_TEST_TIMEOUT_FACTOR (default 1, 2 on Windows and macOS runners).
//   - On timeout it dumps what a flake needs to be diagnosed from one log:
//     the diagnostics registered for the test (service.json, wanted.json,
//     run/ listing, service.log tail, Diagnose text) and a goroutine dump.
//   - Tests assert ordering and state, not elapsed time.

// timeoutFactor scales every waitUntil/bounded wait budget.
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

// scaled is d times timeoutFactor.
func scaled(d time.Duration) time.Duration { return time.Duration(float64(d) * timeoutFactor()) }

var testDiags sync.Map // *testing.T -> []func() string

// diagFor registers a diagnostics dump for t (used by waitUntil on timeout).
func diagFor(t *testing.T, f func() string) {
	t.Helper()
	var l []func() string
	if v, ok := testDiags.Load(t); ok {
		l = v.([]func() string)
	}
	testDiags.Store(t, append(l, f))
	t.Cleanup(func() { testDiags.Delete(t) })
}

// ctrlDiag describes the run/ state of ctrl for a failure message.
func ctrlDiag(ctrl Control) string {
	var b strings.Builder
	if ents, err := os.ReadDir(ctrl.Run()); err == nil {
		for _, e := range ents {
			fi, _ := e.Info()
			size := int64(-1)
			if fi != nil {
				size = fi.Size()
			}
			fmt.Fprintf(&b, "run/%s (%d bytes)\n", e.Name(), size)
		}
	} else {
		fmt.Fprintf(&b, "run/: %v\n", err)
	}
	for _, name := range []string{"service.json", "wanted.json", "record.json"} {
		if data, err := os.ReadFile(filepath.Join(ctrl.Run(), name)); err == nil {
			fmt.Fprintf(&b, "--- %s\n%s\n", name, data)
		}
	}
	fmt.Fprintf(&b, "--- Diagnose\n%s\n", Diagnose(ctrl).Text(ctrl))
	fmt.Fprintf(&b, "--- service.log tail\n%s\n", logTail(ctrl))
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

// waitUntil polls cond (10-50 ms) until it holds or scaled(timeout) passes;
// on timeout it fails with the diagnostics dump.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	budget := scaled(timeout)
	start := time.Now()
	deadline := start.Add(budget)
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

// waitDone waits for ch to close with its own scaled budget and diagnostics.
func waitDone(t *testing.T, what string, timeout time.Duration, ch <-chan struct{}) {
	t.Helper()
	waitUntil(t, what, timeout, func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	})
}

// notDeadWithin fails if the holder dies inside the observation window. It
// returns as soon as the holder is reaped, so only a wrong signal costs the
// whole window; the window is a negative observation ("never signalled"),
// not a wait for something to happen.
func notDeadWithin(t *testing.T, h *holder, window time.Duration, msg string) {
	t.Helper()
	if h.waitDead(window) {
		t.Fatalf("%s\n%s", msg, dumpDiags(t))
	}
}
