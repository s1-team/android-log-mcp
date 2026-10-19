package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `export` goes through the service (started detached when none runs) and
// writes --out; refusals and --force (issue #172).
func TestExportCLIThroughService(t *testing.T) {
	heavyParallel(t)
	env := isoEnv(t)
	ctrl := controlDirOf(env)
	seedStore(t, ctrl)
	dir := t.TempDir()
	out := filepath.Join(dir, "log.txt")

	stdout, errOut, code := cliEnv(t, env, "export", "--device", "emu-1", "--out", out)
	if code != 0 || !strings.HasPrefix(stdout, "ok view=export ") || !strings.Contains(stdout, "file=") || !strings.Contains(stdout, "note code=unredacted") {
		t.Fatalf("export: %d\n%s\n%s", code, stdout, errOut)
	}
	if strings.Contains(stdout, "expires=") || strings.Contains(stdout, "hello report") {
		t.Fatalf("a --out file is never swept and the response has no log text:\n%s", stdout)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "hello report") || !strings.Contains(string(b), "# end ") || !strings.Contains(string(b), "# WARNING: not redacted") {
		t.Fatalf("file: %v\n%s", err, b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("a temp file was left in %s: %v", dir, ents)
	}

	// an existing file needs --force
	stdout, _, code = cliEnv(t, env, "export", "--device", "emu-1", "--out", out)
	if code != 1 || !strings.HasPrefix(stdout, "error code=exists ") || !strings.Contains(stdout, "--force") {
		t.Fatalf("existing: %d\n%s", code, stdout)
	}
	if b2, _ := os.ReadFile(out); string(b2) != string(b) {
		t.Fatal("the existing file changed without --force")
	}
	if stdout, _, code = cliEnv(t, env, "export", "--device", "emu-1", "--out", out, "--force", "--redact"); code != 0 || !strings.Contains(stdout, "redacted=1") {
		t.Fatalf("--force: %d\n%s", code, stdout)
	}
	if b3, _ := os.ReadFile(out); !strings.Contains(string(b3), "# redacted=1") {
		t.Fatalf("not overwritten:\n%s", b3)
	}

	// refusals: nothing is written
	for _, c := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"relative", []string{"--out", "rel.txt"}, 2, "must be an absolute path"},
		{"force without out", []string{"--force"}, 2, "only with --out"},
		{"extra without redact", []string{"--redact-extra", "x"}, 2, "need --redact"},
		{"inside run", []string{"--out", filepath.Join(ctrl, "run", "x.txt")}, 1, "inside android-log-mcp's own data"},
		{"inside store", []string{"--out", filepath.Join(ctrl, "v1", "emu-1", "x.txt")}, 1, "inside android-log-mcp's own data"},
		{"inside export", []string{"--out", filepath.Join(ctrl, "export", "export-x.txt")}, 1, "inside android-log-mcp's own data"},
		{"no directory", []string{"--out", filepath.Join(dir, "nope", "x.txt")}, 1, "the directory does not exist"},
		{"directory", []string{"--out", dir, "--force"}, 1, "is a directory"},
	} {
		stdout, errOut, code := cliEnv(t, env, append([]string{"export", "--device", "emu-1"}, c.args...)...)
		if code != c.code || !strings.Contains(stdout+errOut, c.want) {
			t.Errorf("%s: exit %d, want %d containing %q\n%s%s", c.name, code, c.code, c.want, stdout, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(ctrl, "run", "x.txt")); err == nil {
		t.Fatal("wrote inside run/")
	}

	// without --out: the managed directory, like the tool
	stdout, _, code = cliEnv(t, env, "export", "--device", "emu-1")
	if code != 0 || !strings.Contains(stdout, "expires=") || !strings.Contains(stdout, "export-") {
		t.Fatalf("managed export: %d\n%s", code, stdout)
	}
}

// --offline DIR export needs --out and never writes into the dataset.
func TestExportCLIOffline(t *testing.T) {
	heavyParallel(t)
	env := isoEnv(t)
	root := t.TempDir()
	seedStore(t, root)
	out := filepath.Join(t.TempDir(), "o.txt")

	_, errOut, code := cliEnv(t, env, "--offline", root, "export", "--device", "emu-1")
	if code != 2 || !strings.Contains(errOut, "required with --offline") {
		t.Fatalf("no --out: %d\n%s", code, errOut)
	}
	_, errOut, code = cliEnv(t, env, "--offline", root, "export", "--device", "emu-1", "--out", filepath.Join(root, "x.txt"))
	if code != 2 || !strings.Contains(errOut, "inside the --offline dataset") {
		t.Fatalf("inside dataset: %d\n%s", code, errOut)
	}
	stdout, errOut, code := cliEnv(t, env, "--offline", root, "export", "--device", "emu-1", "--out", out)
	if code != 0 || !strings.HasPrefix(stdout, "ok view=export ") {
		t.Fatalf("offline export: %d\n%s\n%s", code, stdout, errOut)
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), "hello report") {
		t.Fatalf("file: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(root, "export")); err == nil {
		t.Fatal("offline export created export/ in the dataset")
	}
}
