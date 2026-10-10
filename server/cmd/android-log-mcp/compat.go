package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/compat"
	"github.com/s1crab/android-log-mcp/server/internal/device"
)

// runCompat probes one device for the features android-log-mcp depends on
// (read-only: getprop, logcat -d/-g, ps, pm list, dumpsys dropbox, help text).
func runCompat(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("compat", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: android-log-mcp compat [--serial S] [--json | --raw]

Probes a connected device (read-only) for the adb/logcat/ps/pm/dumpsys
features android-log-mcp relies on, evaluates the output with the recorder's
own parsers and prints a compatibility summary. Exit code 0 = supported
(possibly degraded), 1 = a required feature is missing, 2 = usage/adb error.

`)
		fs.PrintDefaults()
	}
	serial := fs.String("serial", os.Getenv("ANDROID_SERIAL"), "device serial (default $ANDROID_SERIAL, or the only connected device)")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	raw := fs.Bool("raw", false, "print the raw probe output (for bug reports / test fixtures) instead of the summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := device.FindADB()
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp compat:", err)
		return 2
	}
	adb := &device.ADB{Path: p}
	out, err := adb.Output(ctx, 10*time.Second, "devices")
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp compat: adb devices:", err)
		return 2
	}
	var online []string
	for _, d := range device.ParseDevices(out) {
		if d.State == "device" {
			online = append(online, d.Serial)
		}
	}
	if *serial == "" {
		if len(online) != 1 {
			fmt.Fprintf(os.Stderr, "android-log-mcp compat: %d devices online (%s); pick one with --serial\n", len(online), strings.Join(online, ", "))
			return 2
		}
		*serial = online[0]
	}
	sh, err := adb.Shell(ctx, *serial, 60*time.Second, compat.Script)
	if err != nil && !strings.Contains(sh, "@@end") {
		fmt.Fprintf(os.Stderr, "android-log-mcp compat: adb -s %s shell: %v\n", *serial, err)
		return 2
	}
	if *raw {
		fmt.Print(sh)
		return 0
	}
	r := compat.Parse(sh)
	r.Serial = *serial
	if v, err := adb.Output(ctx, 5*time.Second, "version"); err == nil {
		r.ADB = adbVersion(v)
	}
	r.Tracking = probeTracking(ctx, p)
	if *asJSON {
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		e.SetEscapeHTML(false)
		e.Encode(r)
	} else {
		fmt.Print(r.Text())
	}
	if strings.HasPrefix(r.Summary, "NOT SUPPORTED") {
		return 1
	}
	return 0
}

// adbVersion: "1.0.41 (platform-tools 37.0.1-15733141)".
func adbVersion(out string) string {
	var proto, tools string
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if v, ok := strings.CutPrefix(ln, "Android Debug Bridge version "); ok {
			proto = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(ln, "Version "); ok {
			tools = strings.TrimSpace(v)
		}
	}
	if tools != "" {
		return proto + " (platform-tools " + tools + ")"
	}
	return proto
}

// probeTracking checks that `adb track-devices` delivers a first frame
// (otherwise the recorder polls `adb devices` every 2 s).
func probeTracking(ctx context.Context, adbPath string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, adbPath, "track-devices")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return "unavailable: " + err.Error()
	}
	got := make(chan int, 1)
	go device.ReadTrackFrames(stdout, func(d []device.DevState) {
		select {
		case got <- len(d):
		default:
		}
	})
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	select {
	case n := <-got:
		return fmt.Sprintf("ok (first frame: %d device(s))", n)
	case <-ctx.Done():
		return "no frame within 3 s (the recorder falls back to polling `adb devices`)"
	}
}
