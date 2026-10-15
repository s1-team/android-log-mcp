package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/report"
)

// logTailBytes bounds the service log in a report.
const logTailBytes = 1 << 20

// tailFile returns the last max bytes of the file (all of it if smaller).
func tailFile(p string, max int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	n := fi.Size()
	if n > max {
		n = max
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, fi.Size()-n); err != nil && n > 0 {
		return ""
	}
	return string(buf)
}

// diagnostics gathers the files every report carries (everything is
// redacted by report.Build): the service log, crash log, service.json, a
// diagnosis of the service lock, and `adb devices -l`. Nothing here talks to
// a device or starts the service; a missing piece is simply left out.
// ctrl may be zero (a dataset that is not this user's).
func diagnostics(ctx context.Context, root string, ctrl Control, adb *device.ADB, logf func(string, ...any)) []report.Extra {
	var out []report.Extra
	add := func(name, text string) {
		if text != "" {
			out = append(out, report.Extra{Name: name, Text: text})
		}
	}
	lp := filepath.Join(LogDir(root), "service.log")
	cur := tailFile(lp, logTailBytes)
	if rest := logTailBytes - len(cur); rest > 0 {
		cur = tailFile(lp+".1", int64(rest)) + cur
	}
	add("service.log", cur)
	add("crash.log", tailFile(filepath.Join(LogDir(root), "crash.log"), 256<<10))
	if ctrl.Dir != "" {
		if b, err := os.ReadFile(ctrl.StatePath()); err == nil {
			add("service.json", string(b))
		}
		add("diagnose.txt", diagnoseText(ctx, ctrl))
	}
	if adb != nil {
		if txt, err := adb.Output(ctx, 5*time.Second, "devices", "-l"); err == nil {
			add("adb-devices.txt", txt)
		} else {
			logf("adb devices -l failed (%v): not in the report", err)
		}
	}
	return out
}

// diagnoseText describes the service lock and socket for a report.
func diagnoseText(ctx context.Context, ctrl Control) string {
	d := Diagnose(ctrl)
	var b strings.Builder
	fmt.Fprintf(&b, "control dir: %s\n", ctrl.Dir)
	fmt.Fprintf(&b, "service lock (%s): %s\n", ctrl.LockPath(), d.Text(ctrl))
	if d.HaveState {
		fmt.Fprintf(&b, "service.json: pid %d, alive %v, ours %v (strong %v), in-process %v, stopping per log %v, version %s", d.PID, d.Alive, d.Ours, d.Strong, d.Embedded, d.Stopping, d.Version)
		if d.Why != "" {
			fmt.Fprintf(&b, ", note: %s", d.Why)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "spawn lock (%s) held: %v\n", ctrl.SpawnLockPath(), lockHeld(ctrl.SpawnLockPath()))
	_, serr := os.Lstat(ctrl.Socket())
	fmt.Fprintf(&b, "socket %s present: %v\n", ctrl.Socket(), serr == nil)
	if d.Held {
		pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		st := probe(pctx, ctrl, time.Second, 2*time.Second)
		fmt.Fprintf(&b, "hello probe: %s\n", map[probeState]string{probeAnswers: "answers", probeAbsent: "no listener", probeSilent: "connects, never answers"}[st])
	}
	return b.String()
}
