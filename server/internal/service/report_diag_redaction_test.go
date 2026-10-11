package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every file under diagnostics/ is redacted on its own: device serials and
// the user's home paths never reach the zip, and each file keeps its
// structure (placeholders replace the secret, the rest stays readable).
// Covers service.log, crash.log, service.json, diagnose.txt, adb-devices.txt.
func TestReportDiagnosticsFilesRedacted(t *testing.T) {
	adb := fakeADBFor(t, "SERIALAAA111", "SERIALBBB222")
	base, err := os.MkdirTemp("", "almd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	home := filepath.Join(base, "homeuserxyz")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ctrl := Control{Dir: filepath.Join(home, "cache", "android-log-mcp")}
	root := filepath.Join(home, "data")
	for _, d := range []string{LogDir(root), ctrl.Run()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st := State{PID: 0, Exe: `C:\Users\WinUserQ\bin\android-log-mcp.exe`, Root: "/home/linuxuserq/data", Socket: "/Users/macuserq/run/s.sock", Version: "0.0.1"}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(ctrl.StatePath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(LogDir(root), "service.log"),
		[]byte("device up serial=SERIALAAA111 root "+root+"\nopen /home/linuxuserq/.cache/x\n"), 0o644)
	os.WriteFile(filepath.Join(LogDir(root), "crash.log"),
		[]byte("panic serial: SERIALBBB222\n\tat /Users/macuserq/src/x.go:1\n\tat C:\\Users\\WinUserQ\\go\\x.go:2\n"), 0o644)
	out := filepath.Join(t.TempDir(), "r.zip")
	if _, err := BuildReport(context.Background(), root, "0.5.0", ReportArgs{Out: out, LogsOnly: true, DevicesList: true, Control: ctrl}, adb); err != nil {
		t.Fatal(err)
	}
	files := readZip(t, out)
	secrets := []string{"SERIALAAA111", "SERIALBBB222", "homeuserxyz", "linuxuserq", "macuserq", "WinUserQ"}
	cases := []struct {
		file string
		want []string // must survive or appear
	}{
		{"diagnostics/service.log", []string{"device up ", "[REDACTED:device_id#", "[REDACTED:home_dir#"}},
		{"diagnostics/crash.log", []string{"panic ", "[REDACTED:device_id#", "x.go:1", "x.go:2"}},
		{"diagnostics/service.json", []string{`"pid"`, `"version"`}},
		{"diagnostics/diagnose.txt", []string{"control dir: ", "service lock"}},
		{"diagnostics/adb-devices.txt", []string{"List of devices attached", "[REDACTED:device_id#1]", "[REDACTED:device_id#2]"}},
	}
	for _, c := range cases {
		txt, ok := files[c.file]
		if !ok {
			t.Errorf("%s missing from the report", c.file)
			continue
		}
		for _, s := range secrets {
			if strings.Contains(txt, s) {
				t.Errorf("%s leaks %q:\n%s", c.file, s, txt)
			}
		}
		for _, w := range c.want {
			if !strings.Contains(txt, w) {
				t.Errorf("%s lacks %q:\n%s", c.file, w, txt)
			}
		}
	}
	// each serial of `adb devices -l` is masked exactly once
	ad := files["diagnostics/adb-devices.txt"]
	if strings.Count(ad, "[REDACTED:device_id#") != 2 {
		t.Errorf("adb-devices.txt: want exactly 2 masked serials:\n%s", ad)
	}
}
