package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCommand(t *testing.T) {
	// No service spawn: plain Parallel so Windows -race does not queue this
	// behind windowsRaceHeavy (#267 wall-clock under count=20).
	t.Parallel()
	// its own home: install is not idempotent across -count runs in one test home
	env := isoEnv(t)
	out, errOut, code := cli(t, env, "install", "--json")
	var r struct {
		Path, Version string
		Updated       bool
	}
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || !r.Updated {
		t.Fatalf("install: %d %q %q", code, out, errOut)
	}
	if filepath.Dir(filepath.Dir(r.Path)) != controlDirOf(env) || filepath.Base(filepath.Dir(r.Path)) != "exe" {
		t.Fatalf("installed to %s, want <control dir>/exe/", r.Path)
	}
	// the copy is a working binary of the same version
	v := exec.Command(r.Path, "--version")
	v.Env = append(os.Environ(), "ALM_RUN_MAIN=1")
	got, err := v.Output()
	if err != nil || strings.TrimSpace(string(got)) != r.Version {
		t.Fatalf("%s --version: %q %v, want %s", r.Path, got, err, r.Version)
	}
	out, errOut, code = cli(t, env, "install")
	if code != 0 || strings.TrimSpace(out) != r.Path || !strings.Contains(errOut, "already up to date") || !strings.Contains(errOut, `"command":`) {
		t.Fatalf("second install: %d %q %q", code, out, errOut)
	}
	out, _, _ = cli(t, env, "root", "--json")
	if !strings.Contains(out, `"exe"`) {
		t.Fatalf("root --json does not show the installed exe: %s", out)
	}
	if _, _, code := cli(t, env, "install", "extra"); code != 2 {
		t.Fatalf("install extra: code %d", code)
	}
}
