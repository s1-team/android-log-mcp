package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

func TestInstallCommand(t *testing.T) {
	out, errOut, code := cli(t, nil, "install", "--json")
	var r struct {
		Path, Version string
		Updated       bool
	}
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || !r.Updated {
		t.Fatalf("install: %d %q %q", code, out, errOut)
	}
	if filepath.Dir(filepath.Dir(r.Path)) != layout.DefaultDir() || filepath.Base(filepath.Dir(r.Path)) != "exe" {
		t.Fatalf("installed to %s, want <control dir>/exe/", r.Path)
	}
	// the copy is a working binary of the same version
	v := exec.Command(r.Path, "--version")
	v.Env = append(os.Environ(), "ALM_RUN_MAIN=1")
	got, err := v.Output()
	if err != nil || strings.TrimSpace(string(got)) != r.Version {
		t.Fatalf("%s --version: %q %v, want %s", r.Path, got, err, r.Version)
	}
	out, errOut, code = cli(t, nil, "install")
	if code != 0 || strings.TrimSpace(out) != r.Path || !strings.Contains(errOut, "already up to date") || !strings.Contains(errOut, `"command":`) {
		t.Fatalf("second install: %d %q %q", code, out, errOut)
	}
	out, _, _ = cli(t, nil, "root", "--json")
	if !strings.Contains(out, `"exe"`) {
		t.Fatalf("root --json does not show the installed exe: %s", out)
	}
	if _, _, code := cli(t, nil, "install", "extra"); code != 2 {
		t.Fatalf("install extra: code %d", code)
	}
}
