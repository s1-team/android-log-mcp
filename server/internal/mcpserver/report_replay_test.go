package mcpserver

// replay-from-report: a report built from the API 29 captures must load back
// (a) as a data directory (--offline <report>/store) and (b) as raw logcat +
// DropBox text replayed through device.Pipeline, with the same crashes and
// launches as the original recording, and with the injected secrets gone.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/report"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// fake secrets of every kind the redactor covers (all synthetic)
var leakSecrets = []struct{ typ, msg, secret string }{
	{"token", "Authorization: Bearer abcDEF123456ghiJKL789xyz", "abcDEF123456ghiJKL789xyz"},
	{"jwt", "id_token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
	{"secret", "using key gh" + "p_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789", "gh" + "p_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789"},
	{"secret", "aws AK" + "IAIOSFODNN7EXAMPLE configured", "AK" + "IAIOSFODNN7EXAMPLE"},
	{"credential", `login {"password":"Tr0ub4dor&3","user":"bob"}`, "Tr0ub4dor&3"},
	{"credential", "GET https://api.example.com/v1?api_key=k3y-9f8e7d6c5b&page=2", "k3y-9f8e7d6c5b"},
	{"email", "signed in as leak.test@example.com", "leak.test@example.com"},
	{"phone", "sms code sent to 13912345678", "13912345678"},
	{"id_card", "kyc id 11010519491231002X ok", "11010519491231002X"},
	{"bank_card", "paid with card 4111 1111 1111 1111", "4111 1111 1111 1111"},
	{"ipv4", "connecting to 203.0.113.77:443", "203.0.113.77"},
	{"domain", "GET https://api.leak-replay.com:8443/v1/login", "api.leak-replay.com"},
	{"domain", "resolving sub.leakreplay.cn", "sub.leakreplay.cn"},
}

func leakLines(sec int64) string {
	var b strings.Builder
	for i, l := range leakSecrets {
		fmt.Fprintf(&b, "         %d.%06d  2000  9%03d  9%03d I LeakTest: %s\n", sec, 1000*(i+1), i, i, l.msg)
	}
	return b.String()
}

func fakeShell(t *testing.T, devNow time.Time) report.ShellFunc {
	return func(ctx context.Context, serial, cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "getprop ro.build.fingerprint"):
			return "google/sdk_gphone_x86_64/generic_x86_64:10/QSR1.211112.011/13135432:userdebug/dev-keys\nAndroid SDK built for x86_64\nGoogle\n10\n29\nAsia/Kuching\n+0800\n07a6d2d3-0000-4000-8000-000000000000\n" + fmt.Sprint(devNow.Unix()) + "\n", nil
		case strings.HasPrefix(cmd, "dumpsys dropbox --print "):
			tag := strings.TrimPrefix(cmd, "dumpsys dropbox --print ")
			b, err := os.ReadFile(filepath.Join("..", "testdata", "api29", "dropbox-"+tag+".txt"))
			if err != nil {
				return "Drop box contents: 0 entries\n", nil
			}
			return string(b), nil
		}
		return "", fmt.Errorf("unexpected command %q", cmd)
	}
}

func readZipDir(t *testing.T, zipPath string) string {
	t.Helper()
	dir := t.TempDir()
	if err := report.Extract(zipPath, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func allFiles(t *testing.T, dir string) string {
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			x, _ := os.ReadFile(p)
			b.Write(x)
			b.WriteByte('\n')
		}
		return nil
	})
	return b.String()
}

func TestReplayFromReport(t *testing.T) {
	e := newReplay(t, fxBoot1)
	e.replay(fixtureFile(t, "session-raw.log") + "\n" + leakLines(1791573034))
	for _, f := range []string{"dropbox-data_app_crash.txt", "dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt", "dropbox-data_app_anr.txt"} {
		e.pipe.ApplyDropbox(fixtureFile(t, f))
	}
	wantCrashes := crashes(e)
	queries := [][2]string{
		{"logcat_crashes", `{"package":"com.example.logtest"}`},
		{"logcat_crashes", `{"package":"com.example.logtest","id":"cr1"}`},
		{"logcat_app_launches", `{"package":"com.example.logtest","from":"2026-10-10 02:50:00"}`},
		{"logcat_search", `{"filter":"tag=:LogTest","from":"2026-10-10 02:50:00","limit":50}`},
	}
	ref := map[string]string{}
	for _, q := range queries {
		ref[q[0]+q[1]] = e.ok(q[0], q[1])
	}

	out := filepath.Join(t.TempDir(), "r.zip")
	devNow := time.Date(2026, 10, 10, 3, 12, 0, 0, time.FixedZone("+0800", 8*3600))
	man, err := report.Build(context.Background(), report.Options{
		Base: e.base, From: "2026-10-10 02:50:00", To: "2026-10-10 03:15:00", Out: out, Version: "test",
		Note:  "repro: tap crash button; reporter leak.test@example.com",
		Shell: fakeShell(t, devNow), Online: map[string]bool{fxSerial: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := readZipDir(t, out)
	everything := allFiles(t, dir)

	// --- redaction: every injected secret is gone, each type was counted
	for _, l := range leakSecrets {
		if strings.Contains(everything, l.secret) {
			t.Errorf("secret %q (%s) survived in the report", l.secret, l.typ)
		}
		if man.Redaction.Hits[l.typ] == 0 {
			t.Errorf("no %s hits in manifest: %v", l.typ, man.Redaction.Hits)
		}
	}
	note, _ := os.ReadFile(filepath.Join(dir, "note.txt"))
	if !strings.Contains(string(note), "[REDACTED:email#") {
		t.Errorf("note not redacted: %s", note)
	}
	// the same address in the log and the note maps to the same placeholder
	segs, _ := filepath.Glob(filepath.Join(dir, "store", "v1", fxSerial, "seg-*.jsonl"))
	if len(segs) != 1 {
		t.Fatalf("want one segment, got %v", segs)
	}
	ph := between(string(note), "reporter ", "\n")
	if !strings.Contains(everything, "signed in as "+ph) {
		t.Errorf("placeholder %q of the note not reused in the logs", ph)
	}

	// --- manifest contents
	var m report.Manifest
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	d := m.Devices[0]
	if d.Records == 0 || d.Records != d.RecordsInWindow || d.Crashes != len(wantCrashes) || d.DeviceInfo != "live" {
		t.Errorf("device manifest: %+v (want %d crashes)", d, len(wantCrashes))
	}
	for _, tag := range []string{"data_app_crash", "data_app_native_crash", "SYSTEM_TOMBSTONE", "data_app_anr"} {
		if d.Dropbox[tag] == 0 {
			t.Errorf("dropbox %s missing: %v", tag, d.Dropbox)
		}
	}
	for _, f := range m.Files {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil || st.Size() != f.Bytes {
			t.Errorf("manifest size of %s = %d, file: %v %v", f.Path, f.Bytes, st, err)
		}
	}
	var info report.DeviceInfo
	ib, _ := os.ReadFile(filepath.Join(dir, "devices", fxSerial, "device.json"))
	json.Unmarshal(ib, &info)
	if !strings.HasPrefix(info.Fingerprint, "google/sdk_gphone_x86_64") || info.SDK != "29" || info.TZOffset != "+0800" || info.BootID == "" {
		t.Errorf("device.json: %+v", info)
	}

	// --- (a) load the store directory as a dataset (--offline r/store):
	// tool calls never write anything there
	before := treeState(t, filepath.Join(dir, "store"))
	mgr := device.NewManager(filepath.Join(dir, "store"), store.Config{})
	mgr.NoADB = true
	mgr.Start(context.Background())
	t.Cleanup(mgr.Close)
	tools := &Tools{M: mgr}
	if st, _ := tools.Call(context.Background(), "logcat_status", json.RawMessage(`{}`)); !strings.Contains(st, "mode: offline") {
		t.Errorf("offline status:\n%s", st)
	}
	defer func() {
		if after := treeState(t, filepath.Join(dir, "store")); after != before {
			t.Errorf("--offline wrote to the dataset:\nbefore %s\nafter  %s", before, after)
		}
	}()
	for _, q := range queries {
		got, isErr := tools.Call(context.Background(), q[0], json.RawMessage(q[1]))
		if isErr {
			t.Fatalf("%s %s on the report: %s", q[0], q[1], got)
		}
		if want := ref[q[0]+q[1]]; normalize(got) != normalize(want) {
			t.Errorf("%s %s differs after loading the report:\n--- original\n%s\n--- report\n%s", q[0], q[1], want, got)
		}
	}
	leak, _ := tools.Call(context.Background(), "logcat_search", json.RawMessage(`{"filter":"tag=:LeakTest","from":"2026-10-10 03:10:00"}`))
	if n := strings.Count(leak, "[REDACTED:"); n < len(leakSecrets) {
		t.Errorf("LeakTest lines: %d placeholders:\n%s", n, leak)
	}
	for i := range leakSecrets { // pid/tid survive
		if !strings.Contains(leak, fmt.Sprintf("9%03d", i)) {
			t.Errorf("pid 9%03d missing:\n%s", i, leak)
		}
	}

	// --- (b) replay the raw logcat + DropBox text through the ingest pipeline
	text, err := os.ReadFile(filepath.Join(dir, "devices", fxSerial, "logcat", fxBoot1+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	e2 := newReplay(t, fxBoot1)
	e2.replay(string(text))
	for _, tag := range report.DropboxTags { // the order the recorder fetches them in
		if b, err := os.ReadFile(filepath.Join(dir, "devices", fxSerial, "dropbox", tag+".txt")); err == nil {
			e2.pipe.ApplyDropbox(string(b))
		}
	}
	if got := crashes(e2); strings.Join(got, "\n") != strings.Join(wantCrashes, "\n") {
		t.Errorf("crashes after replaying the report:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCrashes, "\n"))
	}
	for _, q := range queries[2:3] { // launches are recomputed from the replayed lines
		got := e2.ok(q[0], q[1])
		if normalize(got) != normalize(ref[q[0]+q[1]]) {
			t.Errorf("%s after replay differs:\n--- original\n%s\n--- replay\n%s", q[0], ref[q[0]+q[1]], got)
		}
	}
}

// TestReportSizeCapAndPackage: the cap drops the oldest records (recorded in
// the manifest and as a gap), --package keeps the app's lines and system lines
// naming it, and the trimmed report still loads.
func TestReportSizeCapAndPackage(t *testing.T) {
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	e.st.Close()
	out := filepath.Join(t.TempDir(), "r.zip")
	man, err := report.Build(context.Background(), report.Options{
		Base: e.base, From: "2026-10-10 02:50:00", To: "2026-10-10 03:15:00", Out: out, Version: "test",
		Package: fxPkg, MaxBytes: 64 << 10, Now: func() time.Time { return fxNow() },
	})
	if err != nil {
		t.Fatal(err)
	}
	d := man.Devices[0]
	if man.Limits.DroppedRecords == 0 || d.DroppedRecords != man.Limits.DroppedRecords || man.Limits.LogBytes > 64<<10 {
		t.Fatalf("cap not applied: %+v %+v", man.Limits, d)
	}
	if d.DeviceInfo != "recorded meta (device offline)" || len(d.Dropbox) != 0 {
		t.Errorf("offline report: %+v", d)
	}
	dir := readZipDir(t, out)
	seg, _ := os.ReadFile(filepath.Join(dir, "store", "v1", fxSerial, "seg-00000000000000000001.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(seg)), "\n")
	if len(lines) != d.Records {
		t.Fatalf("segment has %d lines, manifest says %d", len(lines), d.Records)
	}
	for i, ln := range lines {
		var r struct {
			S uint64 `json:"s"`
			K string `json:"k"`
			N string `json:"n"`
			M string `json:"m"`
		}
		json.Unmarshal([]byte(ln), &r)
		if r.S != uint64(i+1) {
			t.Fatalf("seq %d at line %d: not renumbered", r.S, i+1)
		}
		if r.K != fxPkg && !strings.HasPrefix(r.N, fxPkg) && !strings.Contains(r.M, fxPkg) {
			t.Fatalf("line not related to %s: %s", fxPkg, ln)
		}
	}
	mgr := device.NewManager(filepath.Join(dir, "store"), store.Config{})
	mgr.NoADB = true
	mgr.Start(context.Background())
	t.Cleanup(mgr.Close)
	tools := &Tools{M: mgr}
	st, isErr := tools.Call(context.Background(), "logcat_status", json.RawMessage(`{}`))
	if isErr || !strings.Contains(st, "report size cap") {
		t.Errorf("status of a capped report should show the cap gap:\n%s", st)
	}
	cr, isErr := tools.Call(context.Background(), "logcat_crashes", json.RawMessage(`{"package":"com.example.logtest"}`))
	if isErr || !strings.Contains(cr, "crash groups") {
		t.Errorf("crashes from a capped report:\n%s", cr)
	}
}

// treeState lists every path under dir with its size and mtime.
func treeState(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
		}
		return nil
	})
	return b.String()
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

// normalize drops lines that legitimately differ between the live store and
// a report (sequence cursors, "now"-relative notes).
func normalize(s string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, "cursor") || strings.Contains(ln, "seq") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
