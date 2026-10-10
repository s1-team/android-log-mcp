// Package device discovers adb devices and records their logcat into stores.
package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ADB wraps the adb binary.
type ADB struct{ Path string }

// ErrNoADB carries the actionable message from design-v3 §4.
var ErrNoADB = errors.New(`adb not found. Install Android platform-tools or set ANDROID_LOG_MCP_ADB=/path/to/adb (also checked: PATH, $ANDROID_HOME/platform-tools, $ANDROID_SDK_ROOT/platform-tools, ~/Library/Android/sdk/platform-tools, ~/Android/Sdk/platform-tools, ~/android-sdk/platform-tools)`)

// FindADB locates adb.
func FindADB() (string, error) {
	exe := "adb"
	if runtime.GOOS == "windows" {
		exe = "adb.exe"
	}
	if p := os.Getenv("ANDROID_LOG_MCP_ADB"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("ANDROID_LOG_MCP_ADB=%s does not exist", p)
	}
	if p, err := exec.LookPath(exe); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	var cands []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			cands = append(cands, filepath.Join(v, "platform-tools", exe))
		}
	}
	if home != "" {
		cands = append(cands,
			filepath.Join(home, "Library", "Android", "sdk", "platform-tools", exe),
			filepath.Join(home, "Android", "Sdk", "platform-tools", exe),
			filepath.Join(home, "AppData", "Local", "Android", "Sdk", "platform-tools", exe),
			filepath.Join(home, "android-sdk", "platform-tools", exe))
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", ErrNoADB
}

// Output runs adb with a timeout and returns stdout.
func (a *ADB) Output(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Path, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if ctx.Err() != nil {
			return string(out), fmt.Errorf("adb %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		return string(out), fmt.Errorf("adb %s: %v: %s", strings.Join(args, " "), err, firstLines(msg, 3))
	}
	return string(out), nil
}

// Shell runs a read-only shell command on a device.
func (a *ADB) Shell(ctx context.Context, serial string, timeout time.Duration, cmd string) (string, error) {
	out, err := a.Output(ctx, timeout, "-s", serial, "shell", cmd)
	return strings.ReplaceAll(out, "\r\n", "\n"), err
}

// Version returns e.g. "1.0.41".
func (a *ADB) Version(ctx context.Context) (string, error) {
	out, err := a.Output(ctx, 5*time.Second, "version")
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "Android Debug Bridge version ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "Android Debug Bridge version ")), nil
		}
	}
	return "?", nil
}

func firstLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, " | ")
}

// DevState is one adb device entry.
type DevState struct {
	Serial string
	State  string
}

// ParseDevices parses `adb devices` output or one track-devices payload.
func ParseDevices(out string) []DevState {
	var res []DevState
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "List of devices") || strings.HasPrefix(ln, "*") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		st := f[1]
		if st == "no" && len(f) > 2 {
			st = "no permissions"
		}
		res = append(res, DevState{Serial: f[0], State: st})
	}
	return res
}

// ReadTrackFrames reads "%04x<payload>" frames from `adb track-devices`.
func ReadTrackFrames(r io.Reader, fn func([]DevState)) error {
	br := bufio.NewReader(r)
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return err
		}
		n, err := strconv.ParseUint(string(hdr), 16, 32)
		if err != nil {
			return fmt.Errorf("bad track-devices frame header %q", hdr)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		fn(ParseDevices(string(buf)))
	}
}

// Watch reports the device list on every change: `adb track-devices` with a
// 2 s `adb devices` polling fallback while tracking is unavailable.
func (a *ADB) Watch(ctx context.Context, fn func([]DevState, error)) {
	backoff := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, a.Path, "track-devices")
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			started := time.Now()
			err = ReadTrackFrames(stdout, func(d []DevState) { fn(d, nil) })
			cmd.Process.Kill()
			cmd.Wait()
			if time.Since(started) > 10*time.Second {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		// fallback polling until the next track attempt
		deadline := time.Now().Add(backoff)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			out, perr := a.Output(ctx, 5*time.Second, "devices")
			if perr != nil {
				fn(nil, perr)
			} else {
				fn(ParseDevices(out), nil)
			}
			sleep(ctx, 2*time.Second)
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
