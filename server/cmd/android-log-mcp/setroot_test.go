package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/service"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

type setRootOut struct {
	OldRoot        string `json:"old_root"`
	NewRoot        string `json:"new_root"`
	Devices        []string
	ServicePID     int  `json:"service_pid"`
	OldRootRemoved bool `json:"old_root_removed"`
	Remained       []string
}

func setRoot(t *testing.T, env []string, args ...string) (setRootOut, string, int) {
	t.Helper()
	out, errOut, code := cliEnv(t, env, append(append([]string{"set-root"}, args...), "--json")...)
	var r setRootOut
	if code == 0 || code == 3 {
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("set-root %v: %v\n%s", args, err, out)
		}
	}
	return r, errOut, code
}

func rootOf(t *testing.T, env []string) string {
	t.Helper()
	out, errOut, code := cliEnv(t, env, "root")
	if code != 0 {
		t.Fatalf("root: %d %s", code, errOut)
	}
	return strings.TrimSpace(out)
}

func serviceRoot(t *testing.T, env []string) (int, string) {
	t.Helper()
	out, _, code := cliEnv(t, env, "service", "status", "--json")
	if code != 0 {
		return 0, ""
	}
	var st struct {
		PID  int    `json:"pid"`
		Root string `json:"root"`
	}
	json.Unmarshal([]byte(out), &st)
	return st.PID, st.Root
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetRoot(t *testing.T) {
	env := isoEnv(t)
	ctrl := controlDirOf(env)
	seedStore(t, ctrl)
	tmp := resolved(t, t.TempDir())
	a := filepath.Join(tmp, "a")

	// no service: the CLI switches itself and starts nothing
	r, errOut, code := setRoot(t, env, a, "--keep-old")
	if code != 0 || r.ServicePID != 0 || r.NewRoot != a || len(r.Devices) != 1 {
		t.Fatalf("no-service set-root: %d %+v\n%s", code, r, errOut)
	}
	if got := rootOf(t, env); got != a {
		t.Fatalf("root %q, want %q", got, a)
	}
	if pid, _ := serviceRoot(t, env); pid != 0 {
		t.Fatalf("set-root started a service (pid %d)", pid)
	}
	st, err := store.OpenReadOnly(filepath.Join(a, "v1", "emu-1"), store.Config{})
	if err != nil || st.LastSeq() != 1 {
		t.Fatalf("carried store: %v last %d", err, st.LastSeq())
	}
	st.Close()
	if _, err := os.Stat(filepath.Join(ctrl, "v1", "emu-1", "meta.json")); err != nil {
		t.Fatalf("--keep-old removed the old data: %v", err)
	}

	// a service (started by a client) records into the configured root
	if _, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 {
		t.Fatalf("call: %d %s", c, e)
	}
	pid, sroot := serviceRoot(t, env)
	if pid == 0 || sroot != a {
		t.Fatalf("service pid %d root %q, want %q", pid, sroot, a)
	}
	if _, err := os.Stat(filepath.Join(a, "log", "service.log")); err != nil {
		t.Fatalf("service log not in the root: %v", err)
	}

	// validation errors change nothing
	other := filepath.Join(tmp, "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "x.txt"), []byte("x"), 0o644)
	for _, c := range []struct{ dir, want string }{
		{"rel/dir", "not an absolute path"},
		{filepath.Join(a, "sub"), "inside the current root"},
		{tmp, "parent of the current root"},
		{a, "already the data root"},
		{other, "not empty"},
	} {
		if _, errOut, code := setRoot(t, env, c.dir, "--keep-old"); code != 1 || !strings.Contains(errOut, c.want) {
			t.Fatalf("set-root %s: %d %q", c.dir, code, errOut)
		}
	}

	// through the service, back to the default root, deleting a
	r, errOut, code = setRoot(t, env, ctrl, "--delete-old")
	if runtime.GOOS == "windows" {
		// the running service binary (a/bin/…) cannot be deleted on
		// Windows: switched, reported (exit 3), retried at the next start
		if code != 3 || r.ServicePID != pid || r.NewRoot != resolved(t, ctrl) || r.OldRootRemoved || len(r.Remained) != 1 || filepath.Base(r.Remained[0]) != "bin" {
			t.Fatalf("service set-root back (windows): %d %+v\n%s", code, r, errOut)
		}
	} else if code != 0 || r.ServicePID != pid || r.NewRoot != resolved(t, ctrl) || !r.OldRootRemoved {
		t.Fatalf("service set-root back: %d %+v\n%s", code, r, errOut)
	}
	if p2, sroot := serviceRoot(t, env); p2 != pid || sroot != resolved(t, ctrl) {
		t.Fatalf("after switch: service %d root %q", p2, sroot)
	}
	if out, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 || !strings.Contains(out, "root: "+resolved(t, ctrl)) {
		t.Fatalf("status after switch: %d %s %s", c, out, e)
	}
	if runtime.GOOS == "windows" {
		// the next service start removes the leftovers
		if _, e, c := cliEnv(t, env, "service", "stop"); c != 0 {
			t.Fatalf("stop: %s", e)
		}
		if _, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 {
			t.Fatalf("call: %d %s", c, e)
		}
		pid, _ = serviceRoot(t, env)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(a, "bin")); os.IsNotExist(err) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("old root still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ctrl, "config.json")); !os.IsNotExist(err) {
		b, _ := os.ReadFile(filepath.Join(ctrl, "config.json"))
		t.Fatalf("config.json for the default root without leftovers: %v %s", err, b)
	}

	// config.lock held elsewhere: a clear timeout, nothing changes
	lock, ok := store.TryLock(filepath.Join(ctrl, "run", "config.lock"))
	if !ok {
		t.Fatal("config.lock busy")
	}
	b := filepath.Join(tmp, "b")
	_, errOut, code = setRoot(t, env, b, "--keep-old", "--timeout", "500ms")
	store.ReleaseLock(lock)
	if code != 1 || !strings.Contains(errOut, "another set-root is in progress") {
		t.Fatalf("locked: %d %s", code, errOut)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("target created despite the timeout: %v", err)
	}
}

// A failure before the commit rolls back: target removed, root unchanged,
// the service records in the old root again.
func TestSetRootRollback(t *testing.T) {
	for _, stage := range []string{"carry", "commit"} {
		t.Run(stage, func(t *testing.T) {
			env := isoEnv(t, "ALM_TEST_SETROOT_FAIL="+stage)
			ctrl := controlDirOf(env)
			seedStore(t, ctrl)
			if _, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 {
				t.Fatalf("call: %d %s", c, e)
			}
			pid, _ := serviceRoot(t, env)
			target := filepath.Join(t.TempDir(), "new")
			_, errOut, code := setRoot(t, env, target, "--delete-old")
			if code != 1 || !strings.Contains(errOut, "injected failure ("+stage+")") || !strings.Contains(errOut, "rolled back") {
				t.Fatalf("set-root: %d %s", code, errOut)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("target left behind: %v", err)
			}
			if got := rootOf(t, env); got != ctrl {
				t.Fatalf("root %q", got)
			}
			if p2, sroot := serviceRoot(t, env); p2 != pid || sroot != ctrl {
				t.Fatalf("service %d/%d root %q", p2, pid, sroot)
			}
			if _, err := os.Stat(filepath.Join(ctrl, "v1", "emu-1", "meta.json")); err != nil {
				t.Fatalf("old data: %v", err)
			}
			if out, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 || !strings.Contains(out, "root: "+ctrl) {
				t.Fatalf("status after rollback: %d %s %s", c, out, e)
			}
		})
	}
}

// Concurrent set-roots are serialized (config.lock, one service); the
// configured root and the service's root agree afterwards. Without a
// service, --delete-old of the default root keeps the control files.
func TestSetRootConcurrentAndDeleteDefault(t *testing.T) {
	env := isoEnv(t)
	ctrl := controlDirOf(env)
	seedStore(t, ctrl)
	tmp := resolved(t, t.TempDir())
	if _, e, c := cliEnv(t, env, "call", "logcat_status"); c != 0 {
		t.Fatalf("call: %d %s", c, e)
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	errs := make([]string, 2)
	for i, d := range []string{"c", "d"} {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			_, errs[i], codes[i] = setRoot(t, env, filepath.Join(tmp, d), "--keep-old")
		}(i, d)
	}
	wg.Wait()
	if codes[0] != 0 && codes[1] != 0 {
		t.Fatalf("both failed: %v %v", codes, errs)
	}
	for i := range codes {
		if codes[i] != 0 && codes[i] != 1 {
			t.Fatalf("exit %d: %s", codes[i], errs[i])
		}
	}
	_, sroot := serviceRoot(t, env)
	if got := rootOf(t, env); got != sroot || (got != filepath.Join(tmp, "c") && got != filepath.Join(tmp, "d")) {
		t.Fatalf("config root %q, service root %q", got, sroot)
	}
	// no service; delete the default root's data: run/ stays
	if _, e, c := cliEnv(t, env, "service", "stop"); c != 0 {
		t.Fatalf("stop: %s", e)
	}
	r, errOut, code := setRoot(t, env, ctrl, "--keep-old")
	if code != 0 || r.ServicePID != 0 {
		t.Fatalf("back: %d %s", code, errOut)
	}
	r, errOut, code = setRoot(t, env, filepath.Join(tmp, "e"), "--delete-old")
	if code != 0 || r.OldRootRemoved {
		t.Fatalf("delete default: %d %+v %s", code, r, errOut)
	}
	for _, p := range []string{"run", "config.json"} {
		if _, err := os.Stat(filepath.Join(ctrl, p)); err != nil {
			t.Fatalf("control file %s removed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ctrl, "v1")); !os.IsNotExist(err) {
		t.Fatalf("v1 kept: %v", err)
	}
	_ = service.ErrConfigBusy
}
