package service

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readZip(t *testing.T, p string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// --logs-only: diagnostics only (service log redacted, lock diagnosis,
// adb devices -l), no recorded data; works with no service running.
func TestReportLogsOnly(t *testing.T) {
	installFake(t, "FAKEA")
	ctrl := testControl(t)
	root := t.TempDir()
	if err := os.MkdirAll(LogDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(LogDir(root), "service.log"), []byte("2026/10/15 service started\n2026/10/15 user bob@example.com asked\n"), 0o644)
	out := filepath.Join(t.TempDir(), "r.zip")
	rep, err := BuildReport(context.Background(), root, "0.5.0", ReportArgs{Out: out, LogsOnly: true, NoADB: true, DevicesList: true, Control: ctrl}, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := readZip(t, out)
	for name := range files {
		if strings.HasPrefix(name, "store/") || strings.HasPrefix(name, "devices/") {
			t.Fatalf("logs-only report contains %s", name)
		}
	}
	lg := files["diagnostics/service.log"]
	if !strings.Contains(lg, "service started") || strings.Contains(lg, "bob@example.com") || !strings.Contains(lg, "[REDACTED:") {
		t.Fatalf("service.log: %q", lg)
	}
	if !strings.Contains(files["diagnostics/diagnose.txt"], "lock free") {
		t.Fatalf("diagnose.txt: %q", files["diagnostics/diagnose.txt"])
	}
	if !strings.Contains(files["diagnostics/adb-devices.txt"], "FAKEA") {
		t.Fatalf("adb-devices.txt: %q", files["diagnostics/adb-devices.txt"])
	}
	if rep.Manifest == nil || !rep.Manifest.Params.LogsOnly || len(rep.Manifest.Devices) != 0 {
		t.Fatalf("manifest %+v", rep.Manifest)
	}
	if !strings.Contains(files["README.txt"], "diagnostics/service.log") {
		t.Fatal("README does not describe diagnostics/")
	}
}

// A hung lock holder shows up in the diagnosis of an offline report.
func TestReportDiagnosesHungHolder(t *testing.T) {
	ctrl := testControl(t)
	startHolder(t, ctrl, "", "silent")
	out := filepath.Join(t.TempDir(), "r.zip")
	if _, err := BuildReport(context.Background(), t.TempDir(), "0.5.0", ReportArgs{Out: out, LogsOnly: true, NoADB: true, Control: ctrl}, nil); err != nil {
		t.Fatal(err)
	}
	files := readZip(t, out)
	d := files["diagnostics/diagnose.txt"]
	if !strings.Contains(d, "this tool's service") || !strings.Contains(d, "never answers") {
		t.Fatalf("diagnose.txt: %q", d)
	}
	if !strings.Contains(files["diagnostics/service.json"], `"pid"`) {
		t.Fatalf("service.json: %q", files["diagnostics/service.json"])
	}
}

// The reporting hint is off by default and only changes the error text.
func TestReportHintInErrors(t *testing.T) {
	for _, on := range []string{"", "0", "1", "true"} {
		t.Run("env="+on, func(t *testing.T) {
			ctrl := testControl(t)
			h := startHolder(t, ctrl, "", "silent,badexe")
			c := NewClient(ClientOptions{Control: ctrl, Version: "0.5.0", Role: "mcp", StartTimeout: 3 * time.Second, NoDetach: true, Repair: fastRepair(),
				Getenv: func(k string) string {
					if k == ReportHintEnv {
						return on
					}
					return ""
				}})
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := c.Connect(ctx)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "service.log") || !strings.Contains(msg, "service stop --force") {
				t.Fatalf("plain tail missing: %s", msg)
			}
			hint := strings.Contains(msg, "Developer reporting is on")
			if hint != ReportHintOn(on) {
				t.Fatalf("env %q: hint present = %v\n%s", on, hint, msg)
			}
			if hint {
				for _, want := range []string{"Ask the user first", "report --logs-only", "Never upload or send the file yourself", "issues/new"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("hint lacks %q:\n%s", want, msg)
					}
				}
			}
			if h.dead() {
				t.Fatal("a foreign process was signalled")
			}
		})
	}
}

// No network code anywhere in the program: nothing imports an HTTP, SMTP or
// RPC client (the service speaks only over its local socket).
func TestNoNetworkImports(t *testing.T) {
	err := filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range []string{`"net/http"`, `"net/smtp"`, `"net/rpc"`, `"net/textproto"`} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s imports %s", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
