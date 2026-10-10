package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/redact"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

var zone = time.FixedZone("+0800", 8*3600)

// base time: 2026-10-10 12:00:00 +0800
var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, zone)

func mkStore(t *testing.T, recs ...*model.Record) string {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(base, "emu-1", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.TZ, m.TZOffset, m.Model, m.SDK = "Asia/Shanghai", "+0800", "Pixel Test", "29"
		m.Sessions = []store.Session{{BootID: "aaaabbbb-cccc", Boot: "aaaabbbb", FirstSeq: 1}}
		m.LastBootID = "aaaabbbb-cccc"
	})
	for _, r := range recs {
		if r.Boot == "" {
			r.Boot = "aaaabbbb"
		}
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return base
}

func rec(min int, pid int, tag, msg string) *model.Record {
	return &model.Record{TimeNs: t0.Add(time.Duration(min) * time.Minute).UnixNano(), UID: 10146, PID: pid, TID: pid, Level: 'I', Tag: tag, Msg: msg, Buffer: "main", Package: "com.example.app", Process: "com.example.app"}
}

func fixedNow() time.Time { return t0.Add(10 * time.Minute) }

func build(t *testing.T, o Options) (*Manifest, string) {
	t.Helper()
	if o.Out == "" {
		o.Out = filepath.Join(t.TempDir(), "r.zip")
	}
	if o.Now == nil {
		o.Now = fixedNow
	}
	man, err := Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Extract(o.Out, dir); err != nil {
		t.Fatal(err)
	}
	return man, dir
}

func read(t *testing.T, p ...string) string {
	b, err := os.ReadFile(filepath.Join(p...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLogcatTextRoundTrip(t *testing.T) {
	for _, r := range []*model.Record{
		{TimeNs: 1791572202589379000, UID: 10146, PID: 6713, TID: 6714, Level: 'E', Tag: "LogTest", Msg: "line one\n\tat com.example.Foo.bar(Foo.java:12)\n"},
		{TimeNs: 1791572202000001000, UID: -1, PID: 1, TID: 1, Level: 'F', Tag: "init", Msg: "no uid column"},
		{TimeNs: 1791572202000002000, UID: 2000, PID: 9, TID: 9, Level: 'W', Tag: "", Msg: "empty tag"},
		{TimeNs: 1791572202000003000, UID: 1000, PID: 9, TID: 9, Level: 'I', Tag: "Tag With Space", Msg: "中文 und Ümlaut [REDACTED:email#1]"},
	} {
		text := LogcatText(r)
		asm := &logcat.Assembler{Loc: zone, Year: 2026}
		var got *model.Record
		for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if x := asm.Push(ln); x != nil {
				got = x
			}
		}
		if x := asm.Flush(); x != nil {
			got = x
		}
		if got == nil || got.TimeNs != r.TimeNs || got.UID != r.UID || got.PID != r.PID || got.TID != r.TID || got.Level != r.Level || got.Tag != r.Tag || got.Msg != r.Msg {
			t.Errorf("round trip:\n%+v\n%q\n%+v", r, text, got)
		}
	}
}

func TestWindowRedactionAndLayout(t *testing.T) {
	base := mkStore(t,
		rec(-200, 100, "Old", "outside the 2h window"),
		rec(-60, 101, "Net", "token=sekr1t-value-123 for leak@example.com"),
		rec(-30, 102, "Net", "again leak@example.com from 203.0.113.5"),
		rec(5, 103, "Ui", "Displayed com.example.app/.Main: +812ms"),
	)
	man, dir := build(t, Options{Base: base, Note: "see leak@example.com"})
	d := man.Devices[0]
	if d.RecordsInWindow != 3 || d.Records != 3 || d.DeviceInfo != "recorded meta (device offline)" {
		t.Fatalf("window: %+v", d)
	}
	seg := read(t, dir, "store", "emu-1", "seg-00000000000000000001.jsonl")
	txt := read(t, dir, "devices", "emu-1", "logcat", "aaaabbbb.txt")
	note := read(t, dir, "note.txt")
	for _, s := range []string{"sekr1t-value-123", "leak@example.com", "203.0.113.5", "outside the 2h window"} {
		if strings.Contains(seg+txt+note, s) {
			t.Errorf("%q in report", s)
		}
	}
	if !strings.Contains(txt, "[REDACTED:email#1] from [REDACTED:ipv4#1]") || !strings.Contains(note, "see [REDACTED:email#1]") {
		t.Errorf("placeholders not stable:\n%s\n%s", txt, note)
	}
	if man.Redaction.Hits["email"] != 3 || man.Redaction.Unique["email"] != 1 || man.Redaction.Hits["credential"] != 1 {
		t.Errorf("hits %v unique %v", man.Redaction.Hits, man.Redaction.Unique)
	}
	// header fields intact
	if !strings.Contains(txt, "10146   102   102 I Net     : again") || !strings.Contains(txt, "Displayed com.example.app/.Main: +812ms") {
		t.Errorf("structure:\n%s", txt)
	}
	var meta store.Meta
	json.Unmarshal([]byte(read(t, dir, "store", "emu-1", "meta.json")), &meta)
	if len(meta.Sessions) != 1 || meta.Sessions[0].FirstSeq != 1 || meta.TZOffset != "+0800" {
		t.Errorf("meta %+v", meta)
	}
	var m2 map[string]any
	if err := json.Unmarshal([]byte(read(t, dir, "manifest.json")), &m2); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "generated_at", "params", "files", "redaction", "limits", "devices"} {
		if _, ok := m2[k]; !ok {
			t.Errorf("manifest lacks %s", k)
		}
	}
	if strings.Contains(read(t, dir, "manifest.json"), "leak@example.com") {
		t.Error("manifest leaks the note")
	}
}

func TestNoRedactAndExtra(t *testing.T) {
	base := mkStore(t, rec(-5, 1, "A", "mail leak@example.com emp-123456"))
	man, dir := build(t, Options{Base: base, Redact: redact.Options{Disabled: true}})
	if !strings.Contains(read(t, dir, "devices", "emu-1", "logcat", "aaaabbbb.txt"), "leak@example.com") || len(man.Warnings) == 0 {
		t.Error("--no-redact must keep values and warn")
	}
	_, dir = build(t, Options{Base: base, Redact: redact.Options{Extra: []string{`emp-\d{6}`}}})
	if s := read(t, dir, "devices", "emu-1", "logcat", "aaaabbbb.txt"); !strings.Contains(s, "[REDACTED:custom#1]") || strings.Contains(s, "emp-123456") {
		t.Errorf("extra rule: %s", s)
	}
	if _, err := Build(context.Background(), Options{Base: base, Out: filepath.Join(t.TempDir(), "x.zip"), Redact: redact.Options{Extra: []string{"("}}}); err == nil {
		t.Error("bad --redact-extra must fail")
	}
}

func TestPackageFilter(t *testing.T) {
	sys := rec(-5, 500, "ActivityManager", "Start proc 4668:com.example.app/u0a146 for activity")
	sys.Package, sys.Process, sys.UID = "", "system_server", 1000
	other := rec(-4, 600, "Other", "unrelated app line")
	other.Package, other.Process = "com.other", "com.other"
	base := mkStore(t, sys, other, rec(-3, 4668, "App", "hello"))
	man, dir := build(t, Options{Base: base, Package: "com.example.app"})
	txt := read(t, dir, "devices", "emu-1", "logcat", "aaaabbbb.txt")
	if man.Devices[0].Records != 2 || strings.Contains(txt, "unrelated") || !strings.Contains(txt, "Start proc 4668") {
		t.Errorf("package filter:\n%s", txt)
	}
}

func TestErrors(t *testing.T) {
	base := mkStore(t, rec(-5, 1, "A", "x"))
	out := filepath.Join(t.TempDir(), "x.zip")
	for name, o := range map[string]Options{
		"unknown serial":   {Serials: []string{"nope"}},
		"bad from":         {From: "yesterday-ish"},
		"to before from":   {From: "12:05", To: "12:01"},
		"injection":        {AmStart: "com.x/.A; reboot", Shell: func(context.Context, string, string) (string, error) { return "", nil }},
		"am-start offline": {AmStart: "com.x/.A"},
	} {
		o.Base, o.Out, o.Now = base, out, fixedNow
		if _, err := Build(context.Background(), o); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: partial zip left behind", name)
		}
	}
	if _, err := Build(context.Background(), Options{Base: t.TempDir(), Out: out}); err == nil {
		t.Error("empty home must fail")
	}
}

func TestLiveDeviceAmStartAndDropbox(t *testing.T) {
	base := mkStore(t, rec(-5, 1, "A", "x"))
	var cmds []string
	shell := func(ctx context.Context, serial, cmd string) (string, error) {
		cmds = append(cmds, cmd)
		switch {
		case strings.HasPrefix(cmd, "getprop"):
			return fmt.Sprintf("fp/x:10/QSR1/1:userdebug/dev-keys\nPixel Test\nGoogle\n10\n29\nAsia/Shanghai\n+0800\naaaabbbb-cccc\n%d\n", fixedNow().Unix()), nil
		case strings.HasPrefix(cmd, "am start"):
			// the recorder stores the launch while am start runs
			st, err := store.Open(base, "emu-1", store.Config{})
			if err != nil || !st.IsWriter() {
				t.Fatalf("open: %v", err)
			}
			r := rec(11, 77, "ActivityTaskManager", "Displayed com.example.app/.Main: +812ms")
			r.Boot = "aaaabbbb"
			st.Append([]*model.Record{r})
			st.Close()
			return "Starting: Intent { cmp=com.example.app/.Main }\nStatus: ok\nLaunchState: COLD\nTotalTime: 812\nWaitTime: 815\nComplete\n", nil
		case cmd == "dumpsys dropbox --print data_app_crash":
			return "Drop box contents: 2 entries\n\n========================================\n2026-10-10 11:58:00 data_app_crash (text, 10 bytes)\nProcess: com.example.app\nuser leak@example.com\n\n========================================\n2026-10-10 08:00:00 data_app_crash (text, 10 bytes)\nProcess: com.example.app\ntoo old\n", nil
		case strings.HasPrefix(cmd, "dumpsys dropbox"):
			return "Drop box contents: 0 entries\n", nil
		}
		return "", fmt.Errorf("unexpected %q", cmd)
	}
	man, dir := build(t, Options{Base: base, Shell: shell, AmStart: "com.example.app/.Main", CatchUp: time.Millisecond, Now: time.Now})
	d := man.Devices[0]
	if d.DeviceInfo != "live" || d.AmStart == "" || d.Dropbox["data_app_crash"] != 1 {
		t.Fatalf("%+v", d)
	}
	if am := read(t, dir, "devices", "emu-1", "am-start.txt"); !strings.Contains(am, "TotalTime: 812") || strings.Contains(am, "no recorder") {
		t.Errorf("am start output:\n%s", am)
	}
	if !strings.Contains(read(t, dir, "devices", "emu-1", "logcat", "aaaabbbb.txt"), "Displayed com.example.app/.Main") {
		t.Error("lines recorded during am start are missing from the report")
	}
	db := read(t, dir, "devices", "emu-1", "dropbox", "data_app_crash.txt")
	if strings.Contains(db, "too old") || strings.Contains(db, "leak@example.com") || !strings.Contains(db, "Process: com.example.app") {
		t.Errorf("dropbox:\n%s", db)
	}
	if !strings.Contains(read(t, dir, "devices", "emu-1", "device.json"), `"ro.build.fingerprint": "fp/x:10/QSR1/1:userdebug/dev-keys"`) {
		t.Error("fingerprint must be kept verbatim")
	}
	if cmds[1] != "am start -W -n 'com.example.app/.Main'" {
		t.Errorf("am start command: %q", cmds)
	}
}
