package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExportName(t *testing.T) {
	ts := time.Date(2026, 10, 17, 9, 12, 5, 0, time.UTC)
	for _, c := range []struct {
		label string
		n     int
		want  string
	}{
		{"avd-api35", 1, "export-avd-api35-20261017-091205.txt"},
		{"avd-api35", 2, "export-avd-api35-20261017-091205-2.txt"},
		{"192.168.1.5:5555", 1, "export-192.168.1.5_3a5555-20261017-091205.txt"},
		{"a b/c\\d", 1, "export-a_20b_2fc_5cd-20261017-091205.txt"},
		{"con", 1, "export-con-20261017-091205.txt"}, // the prefix keeps reserved names out
		{"x.", 1, "export-x-20261017-091205.txt"},
	} {
		got := ExportName(c.label, ts, c.n)
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.label, got, c.want)
		}
		if strings.ContainsAny(got, `<>:"/\|?* `) {
			t.Errorf("%q has an invalid character", got)
		}
	}
	if n := len(ExportName(strings.Repeat("a", 500), ts, 1)); n > 100 {
		t.Errorf("long label: name has %d bytes", n)
	}
}

func TestEnsureCreatesPrivateExportDir(t *testing.T) {
	r := Root{Dir: filepath.Join(t.TempDir(), "root")}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(r.Export())
	if err != nil || !fi.IsDir() {
		t.Fatalf("export dir: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode().Perm())
	}
	if err := r.Ensure(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestAtomicFile(t *testing.T) {
	dir := t.TempDir()
	list := func() []string {
		ents, _ := os.ReadDir(dir)
		var n []string
		for _, e := range ents {
			n = append(n, e.Name())
		}
		return n
	}
	// commit: only the final name remains, content intact
	a, err := NewAtomicFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(a, "hello")
	if got := list(); len(got) != 1 || !strings.HasPrefix(got[0], ".export-") || !strings.HasSuffix(got[0], ".tmp") {
		t.Fatalf("while writing: %v", got)
	}
	final := filepath.Join(dir, "export-a-20261017-091205.txt")
	if err := a.Commit(final, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(final); string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	if got := list(); len(got) != 1 {
		t.Fatalf("after commit: %v", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(final); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
	// existing target without overwrite: ErrExists, original untouched, temp gone
	b, _ := NewAtomicFile(dir)
	fmt.Fprint(b, "other")
	if err := b.Commit(final, false); !errors.Is(err, ErrExists) {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(final); string(got) != "hello" || len(list()) != 1 {
		t.Fatalf("original changed or temp left: %q %v", got, list())
	}
	// overwrite replaces
	c, _ := NewAtomicFile(dir)
	fmt.Fprint(c, "new")
	if err := c.Commit(final, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(final); string(got) != "new" || len(list()) != 1 {
		t.Fatalf("overwrite: %q %v", got, list())
	}
	// abort removes the temp file and is idempotent; commit after abort fails
	d, _ := NewAtomicFile(dir)
	fmt.Fprint(d, "x")
	d.Abort()
	d.Abort()
	if len(list()) != 1 || d.Commit(filepath.Join(dir, "z.txt"), false) == nil {
		t.Fatalf("abort: %v", list())
	}
	// a target in another directory is refused
	e, _ := NewAtomicFile(dir)
	if err := e.Commit(filepath.Join(dir, "sub", "x.txt"), false); err == nil || len(list()) != 1 {
		t.Fatalf("other dir: %v %v", err, list())
	}
	// a killed writer leaves only a temp file, never a .txt
	f, _ := NewAtomicFile(dir)
	fmt.Fprint(f, "partial")
	for _, n := range list() {
		if strings.HasSuffix(n, ".txt") && n != filepath.Base(final) {
			t.Errorf("partial .txt %s", n)
		}
	}
	f.Abort()
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 10, 17, 9, 12, 5, 0, time.UTC)
	name := func(n int) string { return ExportName("dev", ts, n) }
	for i := 1; i <= 3; i++ {
		p, err := FreeName(dir, name)
		if err != nil || filepath.Base(p) != name(i) {
			t.Fatalf("%d: %q %v", i, p, err)
		}
		os.WriteFile(p, nil, 0o600)
	}
}

func exportFiles(t *testing.T, dir string) map[string]bool {
	ents, _ := os.ReadDir(dir)
	m := map[string]bool{}
	for _, e := range ents {
		m[e.Name()] = true
	}
	return m
}

func TestSweepExportRetention(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "export")
	w := func(name string, size int, age time.Duration) {
		write(t, filepath.Join(dir, name), size, now.Add(-age))
	}
	w("export-old-1.txt", 10, 8*24*time.Hour)                // older than 7 days
	w("export-fresh-1.txt", 10, 6*24*time.Hour)              // kept
	w(".export-1.tmp", 10, 2*time.Hour)                      // stale temp
	w(".export-2.tmp", 10, 5*time.Minute)                    // being written
	w("notes.txt", 10, 30*24*time.Hour)                      // foreign
	w("export-nodot", 10, 30*24*time.Hour)                   // foreign (not .txt)
	os.MkdirAll(filepath.Join(dir, "export-dir.txt"), 0o755) // a directory is never touched
	freed, n := SweepExport(dir, now, 0, 0, 0)
	got := exportFiles(t, dir)
	for _, gone := range []string{"export-old-1.txt", ".export-1.tmp"} {
		if got[gone] {
			t.Errorf("%s not removed", gone)
		}
	}
	for _, kept := range []string{"export-fresh-1.txt", ".export-2.tmp", "notes.txt", "export-nodot", "export-dir.txt"} {
		if !got[kept] {
			t.Errorf("%s removed", kept)
		}
	}
	if n != 2 || freed != 20 {
		t.Errorf("freed %d in %d files", freed, n)
	}
	if f, n := SweepExport(filepath.Join(dir, "missing"), now, 0, 0, 0); f != 0 || n != 0 {
		t.Error("missing dir")
	}
}

func TestSweepExportCountAndSize(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	for i := 0; i < 25; i++ { // i=0 is the oldest
		write(t, filepath.Join(dir, fmt.Sprintf("export-d-%02d.txt", i)), 10, now.Add(-time.Duration(25-i)*time.Minute))
	}
	if _, n := SweepExport(dir, now, 0, 0, 0); n != 5 {
		t.Fatalf("count cap removed %d, want 5", n)
	}
	got := exportFiles(t, dir)
	for i := 0; i < 25; i++ {
		if want := i >= 5; got[fmt.Sprintf("export-d-%02d.txt", i)] != want {
			t.Errorf("file %d present=%v", i, !want)
		}
	}
	// size cap: 20 files of 10 bytes, cap 95 bytes -> the 11 oldest go? no: until total <= cap
	if _, n := SweepExport(dir, now, 0, 0, 95); n != 11 {
		t.Fatalf("size cap removed %d, want 11 (200 -> 90 bytes)", n)
	}
	// negative turns a limit off
	if _, n := SweepExport(dir, now, -1, -1, -1); n != 0 {
		t.Errorf("limits off removed %d", n)
	}
}

// A file that cannot be removed (open in an editor on Windows; here a
// directory-protected parent is not portable, so inject through a non-empty
// directory named like an export file is not matched) is simply skipped.
func TestSweepExportSkipsUnremovable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by the open-file test below on Windows")
	}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "export-a.txt"), 10, now.Add(-10*24*time.Hour))
	os.Chmod(dir, 0o500) // no unlink
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, n := SweepExport(dir, now, 0, 0, 0); n != 0 {
		t.Errorf("removed %d", n)
	}
}

func TestSweepExportOpenFileWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open file can be removed on unix")
	}
	now := time.Now()
	dir := t.TempDir()
	open := filepath.Join(dir, "export-open.txt")
	write(t, open, 10, now.Add(-10*24*time.Hour))
	write(t, filepath.Join(dir, "export-closed.txt"), 10, now.Add(-10*24*time.Hour))
	f, err := os.Open(open) // a human has it open in an editor
	if err != nil {
		t.Fatal(err)
	}
	SweepExport(dir, now, 0, 0, 0)
	if _, err := os.Stat(open); err != nil {
		t.Errorf("open file removed: %v", err)
	}
	f.Close()
	if _, n := SweepExport(dir, now, 0, 0, 0); n != 1 {
		t.Errorf("retry removed %d", n)
	}
}

func TestSweepIncludesExportAndTotal(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	write(t, filepath.Join(base, "export", "export-a-1.txt"), 100, now.Add(-time.Hour))
	write(t, filepath.Join(base, "export", "export-b-1.txt"), 100, now.Add(-30*24*time.Hour))
	if got := TotalBytes(base); got != 200 {
		t.Fatalf("TotalBytes %d, want 200 (export/ counts)", got)
	}
	res := Sweep(SweepOptions{Base: base, Now: now})
	if res.Files != 1 || res.Freed != 100 || res.TotalAfter != 100 {
		t.Errorf("%+v", res)
	}
	// injected limits
	res = Sweep(SweepOptions{Base: base, Now: now, ExportMaxFiles: 1, ExportMaxAge: -1, ExportMaxBytes: -1})
	if res.Files != 0 {
		t.Errorf("one file within the cap: %+v", res)
	}
	write(t, filepath.Join(base, "export", "export-c-1.txt"), 100, now.Add(-time.Minute))
	res = Sweep(SweepOptions{Base: base, Now: now, ExportMaxFiles: 1, ExportMaxAge: -1, ExportMaxBytes: -1})
	if res.Files != 1 {
		t.Errorf("count cap: %+v", res)
	}
}

func TestOwnEntryExport(t *testing.T) {
	if !OwnEntry("export", true, false) || !OwnEntry("export", true, true) {
		t.Error("export/ must be removed by set-root --delete-old")
	}
	if OwnEntry("export", false, false) {
		t.Error("a file named export is not ours")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "export", "export-a-1.txt"), 5, time.Time{})
	write(t, filepath.Join(root, "keep.txt"), 5, time.Time{})
	removed, _, _, errs := RemoveOwn(root, false)
	if len(errs) != 0 || len(removed) != 1 || filepath.Base(removed[0]) != "export" {
		t.Errorf("removed %v errs %v", removed, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Error("foreign file removed")
	}
}

func TestProtectedOut(t *testing.T) {
	t.Parallel()
	root, ctrl := t.TempDir(), t.TempDir()
	for _, c := range []struct {
		path string
		bad  bool
	}{
		{filepath.Join(root, "v1", "dev", "x.txt"), true},
		{filepath.Join(root, "export", "export-x.txt"), true},
		{filepath.Join(root, "tmp", "x"), true},
		{filepath.Join(ctrl, "run", "x"), true},
		{filepath.Join(ctrl, "config.json"), true},
		{filepath.Join(root, "notes", "x.txt"), false},
		{filepath.Join(root, "x.txt"), false},
		{filepath.Join(t.TempDir(), "run", "x.txt"), false},
	} {
		if _, bad := ProtectedOut(c.path, root, ctrl); bad != c.bad {
			t.Errorf("%s: protected=%v, want %v", c.path, bad, c.bad)
		}
	}
	// a symlink into the store does not get around it
	link := filepath.Join(t.TempDir(), "l")
	os.MkdirAll(filepath.Join(root, "v1"), 0o755)
	if err := os.Symlink(filepath.Join(root, "v1"), link); err == nil {
		if _, bad := ProtectedOut(filepath.Join(link, "x.txt"), root); !bad {
			t.Error("symlink into v1 not protected")
		}
	}
}

func TestNewestExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, ok := NewestExport(dir); ok {
		t.Fatal("empty dir")
	}
	w := func(name, body string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	w("export-a-1.txt", "x\n# end lines=1 bytes=9 matched=1 exact=true truncated=0 redaction_hits=0 last_cursor=c\n", time.Hour)
	w("export-a-2.txt", "x\n# end lines=1 bytes=9 matched=2 exact=true truncated=max_mb redaction_hits=0 last_cursor=c\n", time.Minute)
	w("other.txt", "x", 0)
	f, ok := NewestExport(dir)
	if !ok || filepath.Base(f.Path) != "export-a-2.txt" || f.Complete {
		t.Fatalf("%+v", f)
	}
	w("export-a-3.txt", "no footer", 0)
	if f, _ = NewestExport(dir); filepath.Base(f.Path) != "export-a-3.txt" || f.Complete {
		t.Fatalf("%+v", f)
	}
}

// A different letter case on a case-insensitive file system (Windows, macOS)
// names the same entry (issue #205).
func TestProtectedOutDifferentCase(t *testing.T) {
	t.Parallel()
	root, ctrl := t.TempDir(), t.TempDir()
	for _, d := range []string{"v1", "run", "tmp", "bin", "log", "export"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.MkdirAll(filepath.Join(ctrl, "run"), 0o755)
	os.WriteFile(filepath.Join(ctrl, "config.json"), []byte("{}"), 0o600)
	// is the temp dir's file system case-insensitive?
	insensitive := false
	if _, err := os.Stat(filepath.Join(root, "RUN")); err == nil {
		insensitive = true
	}
	for _, c := range []struct{ base, path string }{
		{root, filepath.Join(root, "V1", "dev", "x.txt")},
		{root, filepath.Join(root, "Run", "x.txt")},
		{root, filepath.Join(root, "EXPORT", "export-x.txt")},
		{root, filepath.Join(root, "Tmp", "x")},
		{ctrl, filepath.Join(ctrl, "RUN", "service.lock")},
		{ctrl, filepath.Join(ctrl, "Config.JSON")},
	} {
		_, bad := ProtectedOut(c.path, root, ctrl)
		if want := insensitive || foldNames; bad != want {
			t.Errorf("%s: protected=%v, want %v (case-insensitive fs=%v)", c.path, bad, want, insensitive)
		}
	}
	// the base itself spelled in another case
	if insensitive {
		up := strings.ToUpper(root)
		if _, bad := ProtectedOut(filepath.Join(up, "run", "x.txt"), root); !bad {
			t.Errorf("upper-cased base %s not recognised", up)
		}
		if _, bad := ProtectedOut(filepath.Join(strings.ToUpper(root), "notes", "x.txt"), root); bad {
			t.Error("notes/ is not ours")
		}
	}
	// other names stay allowed whatever the case
	if _, bad := ProtectedOut(filepath.Join(root, "Notes", "x.txt"), root, ctrl); bad {
		t.Error("Notes/ is not ours")
	}
}

// A target in another directory: renamed from the temp file's directory
// (issue #206); when the rename fails (another volume) the content is copied
// next to the target and renamed there. No temp file is left anywhere.
func TestAtomicFileOtherDirectory(t *testing.T) {
	tmpDir, outDir := t.TempDir(), t.TempDir()
	names := func(d string) []string {
		ents, _ := os.ReadDir(d)
		var n []string
		for _, e := range ents {
			n = append(n, e.Name())
		}
		return n
	}
	write := func(final string, overwrite bool) error {
		a, err := NewAtomicFile(tmpDir)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(a, "payload")
		return a.Commit(final, overwrite)
	}
	final := filepath.Join(outDir, "x.txt")
	if err := write(final, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(final); string(b) != "payload" || len(names(tmpDir)) != 0 || len(names(outDir)) != 1 {
		t.Fatalf("rename: %q tmp=%v out=%v", b, names(tmpDir), names(outDir))
	}
	if err := write(final, false); !errors.Is(err, ErrExists) || len(names(tmpDir)) != 0 || len(names(outDir)) != 1 {
		t.Fatalf("exists: %v tmp=%v out=%v", err, names(tmpDir), names(outDir))
	}

	// cross-volume: the first rename fails
	old := renameFile
	defer func() { renameFile = old }()
	calls := 0
	renameFile = func(a, b string) error {
		calls++
		if calls == 1 {
			return errors.New("invalid cross-device link")
		}
		return old(a, b)
	}
	if err := write(final, true); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "payload" || calls != 2 || len(names(tmpDir)) != 0 || len(names(outDir)) != 1 {
		t.Fatalf("fallback: %q calls=%d tmp=%v out=%v", b, calls, names(tmpDir), names(outDir))
	}
	// the fallback fails too: nothing is left behind
	renameFile = func(a, b string) error { return errors.New("denied") }
	os.Remove(final)
	if err := write(final, false); err == nil || len(names(tmpDir)) != 0 || len(names(outDir)) != 0 {
		t.Fatalf("failure: %v tmp=%v out=%v", err, names(tmpDir), names(outDir))
	}
}
