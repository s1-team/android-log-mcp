package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func seedStore(t *testing.T, root string) {
	t.Helper()
	st, err := store.Open(root, "emu-1", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.TZ, m.TZOffset, m.Model, m.SDK = "UTC", "+0000", "Pixel Test", "29"
		m.Sessions = []store.Session{{BootID: "aaaabbbb-cccc", Boot: "aaaabbbb", FirstSeq: 1}}
		m.LastBootID = "aaaabbbb-cccc"
	})
	now := time.Now().Add(-time.Minute).UnixNano()
	if err := st.Append([]*model.Record{{TimeNs: now, UID: 10146, PID: 42, TID: 42, Level: 'I', Tag: "T", Msg: "hello report", Buffer: "main", Boot: "aaaabbbb"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// report goes through the service; with none running it starts one
// detached (issue #11 §9), never building it in this process.
func TestReportStartsService(t *testing.T) {
	parallelUnlessWindowsRace(t)
	env := isoEnv(t)
	seedStore(t, controlDirOf(env))
	out1 := filepath.Join(t.TempDir(), "r1.zip")
	_, errOut, code := cliEnv(t, env, "report", "--no-adb", "-o", out1)
	if code != 0 || !strings.Contains(errOut, "report built_by=service pid=") || !strings.Contains(errOut, "report device=emu-1 records=1 ") {
		t.Fatalf("report without a service: %d\n%s", code, errOut)
	}
	if _, err := os.Stat(out1); err != nil {
		t.Fatal(err)
	}
	pid := statePID(t, env)
	if pid == 0 || pid == os.Getpid() {
		t.Fatalf("report did not start a detached service (pid %d)", pid)
	}
	out2 := filepath.Join(t.TempDir(), "r2.zip")
	_, errOut, code = cliEnv(t, env, "report", "--no-adb", "-o", out2)
	if code != 0 || !strings.Contains(errOut, "report built_by=service pid="+strconv.Itoa(pid)+" ") || !strings.Contains(errOut, "report device=emu-1 records=1 ") {
		t.Fatalf("service report: %d\n%s", code, errOut)
	}
	if _, err := os.Stat(out2); err != nil {
		t.Fatal(err)
	}
	// errors from the service reach the caller
	_, errOut, code = cliEnv(t, env, "report", "--no-adb", "--serial", "nope", "-o", filepath.Join(t.TempDir(), "r3.zip"))
	if code != 1 || !strings.Contains(errOut, "nope") {
		t.Fatalf("bad serial: %d\n%s", code, errOut)
	}
}
