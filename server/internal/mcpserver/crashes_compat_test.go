package mcpserver

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// Issue #225 (compatibility, new reads old): crashes.jsonl files written by
// the released 0.9.0 and 0.8.1 binaries (no inc / ev) are listed and shown
// byte-identically to what those binaries printed, and reading rewrites
// nothing. testdata/crashes-golden/README.md says how the files were made.

var reOffset = regexp.MustCompile(`offset_s=-?[0-9.]+`)

func goldenFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "crashes-golden", rel))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(rel, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestNewReadsOldCrashesJSONL(t *testing.T) {
	t.Parallel()
	session := func(e *replayEnv) {
		e.file("java_crash.logcat.txt", "native_crash.logcat.txt", "anr_broadcast.logcat.txt")
		for _, f := range []string{"dropbox-data_app_crash.txt", "dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt", "dropbox-data_app_anr.txt"} {
			e.pipe.ApplyDropbox(fixtureFile(t, f))
		}
	}
	dup := func(e *replayEnv) {
		e.replay(strings.ReplaceAll(fixtureFile(t, "anr_broadcast.logcat.txt"), "1791572770.819708", "1791572900.819708"))
		e.pipe.ApplyDropbox(fixtureFile(t, "dropbox-data_app_anr.txt"))
	}
	for _, tc := range []struct {
		name, crashes, out string
		build              func(e *replayEnv)
	}{
		{"0.9.0 session", "v0.9.0/session.crashes.jsonl.gz", "v0.9.0/session.out.txt", session},
		{"0.9.0 compacted", "v0.9.0/compacted.crashes.jsonl.gz", "v0.9.0/compacted.out.txt", session},
		{"0.9.0 duplicate ANRs", "v0.9.0/dupanr.crashes.jsonl.gz", "v0.9.0/dupanr.out.txt", dup},
		{"0.8.1 session", "v0.8.1/session.crashes.jsonl.gz", "v0.8.1/session.out.txt", session},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newReplay(t, fxBoot1)
			tc.build(e) // the records (segments) and the pipeline's own aux files
			golden := goldenFile(t, tc.crashes)
			// the golden replaces the file written by this build (new inode)
			path := filepath.Join(e.st.Dir, "crashes.jsonl")
			tmp := path + ".golden"
			if err := os.WriteFile(tmp, golden, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
			got := crashOutputs(t, e)
			if want := string(goldenFile(t, tc.out)); got != want {
				t.Errorf("output differs from what the released binary printed\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(after) != sha256.Sum256(golden) {
				t.Errorf("reading the old file rewrote it (%d -> %d bytes)", len(golden), len(after))
			}
		})
	}
}

// crashOutputs prints the list and every detail view like the golden
// generator did.
func crashOutputs(t *testing.T, e *replayEnv) string {
	t.Helper()
	var sb strings.Builder
	var ids []string
	for _, c := range device.LoadCrashes(e.st) {
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		t.Fatal("no crashes")
	}
	call := func(args string) string {
		out, isErr := e.call("logcat_crashes", args)
		if isErr {
			t.Fatalf("%s: %s", args, out)
		}
		return out
	}
	sb.WriteString("=== list\n" + call(`{"package":"com.example.logtest"}`) + "\n")
	sb.WriteString("=== all\n" + call(`{}`) + "\n")
	for _, id := range ids {
		b, _ := json.Marshal(map[string]any{"id": id})
		sb.WriteString("=== detail " + id + "\n" + call(string(b)) + "\n")
	}
	return reOffset.ReplaceAllString(sb.String(), "offset_s=N")
}

// Issue #225 (fmt=1 unchanged): the records of the same captures, ingested
// by this build (which writes inc and ev), print what the released
// binaries printed.
func TestNewWritesTheSameOutputAsOld(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt", "native_crash.logcat.txt", "anr_broadcast.logcat.txt")
	for _, f := range []string{"dropbox-data_app_crash.txt", "dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt", "dropbox-data_app_anr.txt"} {
		e.pipe.ApplyDropbox(fixtureFile(t, f))
	}
	got := crashOutputs(t, e)
	if want := string(goldenFile(t, "v0.9.0/session.out.txt")); got != want {
		t.Errorf("fmt=1 output changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(e.st.Dir, "crashes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"ev":[`)) || !bytes.Contains(raw, []byte(`"inc":"`)) {
		t.Errorf("new facts carry no ev / inc:\n%.600s", raw)
	}
}

// Issue #225 (downgrade, old reads new): a dataset written by this build
// (inc and ev on the facts) is read by a released binary. It needs that
// binary, so it only runs with ALM_OLD_BIN=<path of a v0.9.x build>; the
// result for the PR is recorded in the PR body.
func TestOldBinaryReadsNewFile(t *testing.T) {
	bin := os.Getenv("ALM_OLD_BIN")
	if bin == "" {
		t.Skip("set ALM_OLD_BIN to a released android-log-mcp binary (v0.9.x)")
	}
	e := newReplay(t, fxBoot1)
	e.file("java_crash.logcat.txt", "native_crash.logcat.txt", "anr_broadcast.logcat.txt")
	for _, f := range []string{"dropbox-data_app_crash.txt", "dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt", "dropbox-data_app_anr.txt"} {
		e.pipe.ApplyDropbox(fixtureFile(t, f))
	}
	e.st.Close()
	raw, err := os.ReadFile(filepath.Join(e.st.Dir, "crashes.jsonl"))
	if err != nil || !bytes.Contains(raw, []byte(`"ev":[`)) {
		t.Fatalf("the new file has no ev: %v", err)
	}
	run := func(args string) string {
		cmd := procattr.Hide(exec.Command(bin, "--offline", e.base, "call", "logcat_crashes", args))
		cache := t.TempDir()
		cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+cache, "HOME="+cache, "LocalAppData="+cache, "ANDROID_LOG_MCP_ADB=/nonexistent-adb")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", args, err, out)
		}
		return string(out)
	}
	var sb strings.Builder
	sb.WriteString("=== list\n" + strings.TrimRight(run(`{"package":"com.example.logtest"}`), "\n") + "\n\n")
	sb.WriteString("=== all\n" + strings.TrimRight(run(`{}`), "\n") + "\n\n")
	for _, id := range []string{"anr1", "cr2", "cr1"} {
		sb.WriteString("=== detail " + id + "\n" + strings.TrimRight(run(`{"id":"`+id+`"}`), "\n") + "\n\n")
	}
	got := reOffset.ReplaceAllString(sb.String(), "offset_s=N")
	want := string(goldenFile(t, "v0.9.0/session.out.txt"))
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Errorf("the released binary prints something else for the new file\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
