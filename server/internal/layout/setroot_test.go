package layout

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func resetConfig(t *testing.T) {
	t.Helper()
	os.Remove(ConfigPath())
	t.Cleanup(func() { os.Remove(ConfigPath()) })
}

func TestConfigResolution(t *testing.T) {
	resetConfig(t)
	if !strings.Contains(ConfigPath(), "alm-testhome-") {
		t.Fatalf("config path %s not isolated", ConfigPath())
	}
	if r, err := ConfiguredRoot(); err != nil || r != DefaultDir() {
		t.Fatalf("default: %q %v", r, err)
	}
	dir := t.TempDir()
	if err := WriteConfig(Config{Root: dir}); err != nil {
		t.Fatal(err)
	}
	if r, err := ConfiguredRoot(); err != nil || r != dir {
		t.Fatalf("configured: %q %v", r, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(ConfigPath())); len(ents) != 1 {
		t.Fatalf("atomic write left files: %v", ents)
	}
	os.WriteFile(ConfigPath(), []byte(`{"root":"relative/dir"}`), 0o644)
	if _, err := ConfiguredRoot(); err == nil {
		t.Fatal("relative root accepted")
	}
	os.WriteFile(ConfigPath(), []byte(`{`), 0o644)
	if _, err := ConfiguredRoot(); err == nil {
		t.Fatal("corrupt config accepted")
	}
}

func TestRemovedHomeEnv(t *testing.T) {
	t.Setenv(RemovedHomeEnv, "")
	if CheckRemovedHome() != nil {
		t.Fatal("empty env rejected")
	}
	t.Setenv(RemovedHomeEnv, "/tmp/x")
	err := CheckRemovedHome()
	if !errors.Is(err, ErrHomeRemoved) || !strings.Contains(err.Error(), "set-root") || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("err %v", err)
	}
}

func TestValidateTarget(t *testing.T) {
	cur := filepath.Join(t.TempDir(), "root")
	os.MkdirAll(cur, 0o755)
	for name, target := range map[string]string{
		"equal":  cur,
		"inside": filepath.Join(cur, "sub"),
		"parent": filepath.Dir(cur),
		"empty":  "",
	} {
		if _, err := ValidateTarget(target, cur); err == nil {
			t.Errorf("%s: %s accepted", name, target)
		}
	}
	foreign := t.TempDir()
	os.WriteFile(filepath.Join(foreign, "notes.txt"), []byte("x"), 0o644)
	if _, err := ValidateTarget(foreign, cur); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("foreign non-empty dir: %v", err)
	}
	marked := t.TempDir()
	os.WriteFile(filepath.Join(marked, MarkerName), nil, 0o644)
	os.MkdirAll(filepath.Join(marked, "v1"), 0o755)
	if _, err := ValidateTarget(marked, cur); err != nil {
		t.Errorf("existing root rejected: %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "a", "b")
	got, err := ValidateTarget(fresh, cur)
	if err != nil || !filepath.IsAbs(got) {
		t.Errorf("new dir: %q %v", got, err)
	}
	// relative paths are made absolute
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	base := t.TempDir()
	os.Chdir(base)
	if got, err := ValidateTarget("rel", cur); err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "rel" {
		t.Errorf("relative: %q %v", got, err)
	}
	os.Chdir(wd)
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		os.MkdirAll(ro, 0o555)
		defer os.Chmod(ro, 0o755)
		if _, err := ValidateTarget(ro, cur); err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Errorf("unwritable: %v", err)
		}
	}
}

func TestSetRootNeedsExactlyOneOption(t *testing.T) {
	resetConfig(t)
	for _, o := range []SetRootOptions{{Target: t.TempDir()}, {Target: t.TempDir(), KeepOld: true, DeleteOld: true}} {
		if _, err := SetRoot(o); err == nil || !strings.Contains(err.Error(), "--keep-old") || !strings.Contains(err.Error(), "--delete-old") {
			t.Fatalf("err %v", err)
		}
	}
	if _, err := os.Stat(ConfigPath()); err == nil {
		t.Fatal("config written without a valid option")
	}
}

// oldRoot builds a root with our structure plus foreign content.
func oldRoot(t *testing.T, foreign bool) string {
	r := filepath.Join(t.TempDir(), "old")
	(Root{Dir: r, OwnersDir: filepath.Join(t.TempDir(), "owners")}).Ensure()
	os.MkdirAll(filepath.Join(r, "v1", "avd-a"), 0o755)
	os.WriteFile(filepath.Join(r, "v1", "avd-a", "seg-00000000000000000001.jsonl"), []byte("{}\n"), 0o644)
	os.MkdirAll(filepath.Join(r, "v2"), 0o755)
	os.WriteFile(filepath.Join(r, "tmp", "x"), nil, 0o644)
	if foreign {
		os.WriteFile(filepath.Join(r, "notes.txt"), []byte("mine"), 0o644)
		os.MkdirAll(filepath.Join(r, "legacy"), 0o755)
		os.MkdirAll(filepath.Join(r, "vx"), 0o755)
	}
	return r
}

func TestSetRootKeepOldImmediate(t *testing.T) {
	resetConfig(t)
	old := oldRoot(t, false)
	WriteConfig(Config{Root: old})
	target := filepath.Join(t.TempDir(), "new")
	res, err := SetRoot(SetRootOptions{Target: target, KeepOld: true, OwnersDir: t.TempDir(), Timeout: time.Second})
	if err != nil || res.Outcome != "switched" || len(res.Moved)+len(res.Waiting) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	c, _ := ReadConfig()
	if c.Root != res.NewRoot || c.Switch != nil {
		t.Fatalf("config %+v", c)
	}
	if _, err := os.Stat(filepath.Join(old, "v1", "avd-a", "seg-00000000000000000001.jsonl")); err != nil {
		t.Fatal("--keep-old touched the old root")
	}
	if _, err := os.Stat(filepath.Join(target, MarkerName)); err != nil {
		t.Fatal("new root not initialised")
	}
}

func TestSetRootDeleteOldOnlyOurs(t *testing.T) {
	resetConfig(t)
	old := oldRoot(t, true)
	WriteConfig(Config{Root: old})
	res, err := SetRoot(SetRootOptions{Target: filepath.Join(t.TempDir(), "new"), DeleteOld: true, OwnersDir: t.TempDir()})
	if err != nil || res.OldRootRemoved {
		t.Fatalf("%+v %v", res, err)
	}
	for _, gone := range []string{"v1", "v2", "tmp", "run", MarkerName} {
		if _, err := os.Stat(filepath.Join(old, gone)); err == nil {
			t.Errorf("%s not deleted", gone)
		}
	}
	for _, kept := range []string{"notes.txt", "legacy", "vx"} {
		if _, err := os.Stat(filepath.Join(old, kept)); err != nil {
			t.Errorf("foreign %s deleted", kept)
		}
		if !slices.Contains(res.Remained, filepath.Join(old, kept)) {
			t.Errorf("%s not reported as remaining: %v", kept, res.Remained)
		}
	}
	// only our structure: the old root itself goes too
	resetConfig(t)
	old2 := oldRoot(t, false)
	WriteConfig(Config{Root: old2})
	res, err = SetRoot(SetRootOptions{Target: filepath.Join(t.TempDir(), "new"), DeleteOld: true, OwnersDir: t.TempDir()})
	if err != nil || !res.OldRootRemoved {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(old2); err == nil {
		t.Fatal("empty old root kept")
	}
}

// A recorder that does not acknowledge (e.g. an older version) makes
// set-root time out without touching the old root; once it moves, running
// the same command again finishes the switch.
func TestSetRootTimeoutAndResume(t *testing.T) {
	resetConfig(t)
	old := oldRoot(t, false)
	WriteConfig(Config{Root: old})
	owners := t.TempDir()
	r := Root{Dir: old, OwnersDir: owners}
	lock, _, err := r.AcquireOwner("avd-a", Owner{PID: 4242, Version: "0.3.0", DataDir: DeviceDir(old, "avd-a"), Root: old})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	target := filepath.Join(t.TempDir(), "new")
	opt := SetRootOptions{Target: target, DeleteOld: true, OwnersDir: owners, Timeout: 300 * time.Millisecond, Poll: 20 * time.Millisecond}
	res, err := SetRoot(opt)
	if !errors.Is(err, ErrSwitchTimeout) || res.Outcome != "timeout" || len(res.Waiting) != 1 || !strings.Contains(res.Waiting[0], "pid 4242") {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(old, "v1", "avd-a")); err != nil {
		t.Fatal("old root touched before the acknowledgement")
	}
	c, _ := ReadConfig()
	if c.Switch == nil || c.Switch.From != res.OldRoot || c.Root != res.NewRoot {
		t.Fatalf("pending marker %+v", c)
	}
	// the recorder moves (acknowledges) while set-root runs again
	go func() {
		time.Sleep(100 * time.Millisecond)
		lock.Update(func(o *Owner) { o.Root, o.DataDir = res.NewRoot, DeviceDir(res.NewRoot, "avd-a") })
	}()
	opt.Timeout = 5 * time.Second
	res2, err := SetRoot(opt)
	if err != nil || res2.Outcome != "switched" || res2.OldRoot != res.OldRoot || len(res2.Moved) != 1 || !res2.OldRootRemoved {
		t.Fatalf("resume: %+v %v", res2, err)
	}
	if c, _ := ReadConfig(); c.Switch != nil {
		t.Fatal("marker not cleared")
	}
}
