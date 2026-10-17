package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// logcat_export tests (issue #172). Fixtures are stores built directly (no
// adb), read by a Manager with NoADB; the tool writes under a temp root.

type exFx struct {
	tools  *Tools
	recs   []*model.Record // arrival order, Seq set by the store
	dir    string          // <root>/export
	devNow time.Time
}

var exTags = []string{"ActivityManager", "OkHttp", "MyApp", "chatty"}
var exLevels = []model.Level{model.Debug, model.Info, model.Warn, model.Error, model.Fatal}

// exMsgs include multi-line, CR, unicode, an empty message and secrets.
func exMsg(i int) string {
	switch i % 9 {
	case 1:
		return fmt.Sprintf("multi %d\n\tat a.B.c(B.java:%d)\r\n\tat d.E.f(E.java:2)", i, i)
	case 2:
		return fmt.Sprintf("日本語 %d ✓", i)
	case 3:
		return ""
	case 4:
		return fmt.Sprintf("user alice%d@example.com logged in token=abcdef0123456789abcdef%d", i, i)
	}
	return fmt.Sprintf("line %d payload", i)
}

func newExFx(t *testing.T, n int, jumpAt ...int) *exFx {
	t.Helper()
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 256 << 10, BlockBytes: 4 << 10, MemRecords: 100}
	st, err := store.Open(base, "emu-x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	devNow := time.Now().UTC().Truncate(time.Second)
	hostNow := time.Now()
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ, m.Model, m.SDK, m.Release = "emu-x", "UTC", "sdk_gphone", "35", "15"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
		m.ClockOffsetNs, m.ClockHostNs = int64(devNow.Sub(hostNow)), hostNow.UnixNano()
	})
	fx := &exFx{devNow: devNow}
	start := devNow.Add(-time.Duration(n+100) * time.Second)
	var jumps []store.ClockJump
	var recs []*model.Record
	for i := 0; i < n; i++ {
		tm := start.Add(time.Duration(i) * time.Second)
		for _, j := range jumpAt {
			if i >= j {
				tm = tm.Add(-time.Hour) // set back by an hour at line j
			}
		}
		recs = append(recs, &model.Record{TimeNs: tm.UnixNano() + int64(i%1000)*1e6, PID: 100 + i%7, TID: 1000 + i%13, Level: exLevels[i%len(exLevels)], Tag: exTags[i%len(exTags)], Msg: exMsg(i), Boot: "b1", UID: 10000 + i%3, Package: "com.ex.a"})
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	for _, j := range jumpAt {
		prev, cur := recs[j-1], recs[j]
		jumps = append(jumps, store.ClockJump{Seq: cur.Seq, DeltaNs: cur.TimeNs - prev.TimeNs, BeforeNs: prev.TimeNs, AfterNs: cur.TimeNs, HostNs: hostNow.UnixNano(), Source: "live"})
	}
	for _, j := range jumps {
		st.AddClockJump(j)
	}
	fx.recs = recs
	st.Close()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	root := layout.Root{Dir: filepath.Join(t.TempDir(), "root")}
	if err := root.Ensure(); err != nil {
		t.Fatal(err)
	}
	fx.tools = &Tools{M: m, ExportRoot: root}
	fx.dir = root.Export()
	return fx
}

func (fx *exFx) run(args string) (string, bool) {
	return fx.tools.Call(context.Background(), "logcat_export", json.RawMessage(args))
}

func (fx *exFx) ok(t *testing.T, args string) string {
	t.Helper()
	out, isErr := fx.run(args)
	if isErr {
		t.Fatalf("export %s: %s", args, out)
	}
	return out
}

var reFile = regexp.MustCompile(`file=("(?:[^"\\]|\\.)*"|\S+)`)

func respFile(t *testing.T, out string) string {
	t.Helper()
	m := reFile.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no file= in %q", out)
	}
	s := m[1]
	if strings.HasPrefix(s, `"`) {
		u, err := strconv.Unquote(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	return s
}

func respKV(out, key string) string {
	m := regexp.MustCompile(`(?:^|\s)` + key + `=(\S+)`).FindStringSubmatch(strings.SplitN(out, "\n", 2)[0])
	if m == nil {
		return ""
	}
	return m[1]
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// body: the lines that are not header, footer or markers.
func bodyLines(content string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if strings.HasPrefix(l, "# ") || strings.HasPrefix(l, "--------- ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// expected body of a set of records: the format of the design, by hand.
func wantLines(recs []*model.Record, redactor func(string) string) []string {
	var out []string
	for _, r := range recs {
		msg := r.Msg
		if redactor != nil {
			msg = redactor(msg)
		}
		for _, ln := range strings.Split(msg, "\n") {
			out = append(out, fmt.Sprintf("%s %5d %5d %c %-8s: %s", time.Unix(0, r.TimeNs).UTC().Format("2006-01-02 15:04:05.000"), r.PID, r.TID, byte(r.Level), r.Tag, strings.TrimRight(ln, "\r")))
		}
	}
	return out
}

func eqLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d lines, want %d\nfirst got %q\nfirst want %q", what, len(got), len(want), first(got), first(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: line %d\n got %q\nwant %q", what, i, got[i], want[i])
		}
	}
}

func first(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func TestExportFormatAndAgreement(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 300)
	out := fx.ok(t, `{}`)
	if len(out) > 800 {
		t.Errorf("response is %d bytes, want about 600:\n%s", len(out), out)
	}
	if !strings.HasPrefix(out, "ok view=export device=emu-x file=") || !strings.Contains(out, "note code=unredacted") || !strings.Contains(out, "fmt=1") {
		t.Fatalf("response:\n%s", out)
	}
	for _, r := range fx.recs[:3] {
		if strings.Contains(out, r.Tag+":") {
			t.Errorf("response has log lines:\n%s", out)
		}
	}
	p := respFile(t, out)
	if !filepath.IsAbs(p) || filepath.Dir(p) != fx.dir {
		t.Errorf("file %q not in %s", p, fx.dir)
	}
	if !regexp.MustCompile(`^export-emu-x-\d{8}-\d{6}\.txt$`).MatchString(filepath.Base(p)) {
		t.Errorf("name %q", filepath.Base(p))
	}
	content := readFile(t, p)
	eqLines(t, "body", bodyLines(content), wantLines(fx.recs, nil))
	// header / footer / response agree
	for _, h := range []string{"# android-log-mcp export fmt=1 version=", "# device=emu-x model=sdk_gphone api=35 release=15 boot=b1", "# redacted=0", "# WARNING: not redacted", "# crashes=0 anrs=0", "# caps max_mb=64 time_budget_s=20", "# ---\n"} {
		if !strings.Contains(content, h) {
			t.Errorf("header lacks %q:\n%s", h, content[:min(len(content), 1500)])
		}
	}
	foot := content[strings.LastIndex(content, "# end "):]
	for _, k := range []string{"lines", "bytes", "matched", "exact"} {
		if a, b := respKV(out, k), regexp.MustCompile(k+`=(\S+)`).FindStringSubmatch(foot); b == nil || a != b[1] {
			t.Errorf("%s: response %q, footer %q", k, a, foot)
		}
	}
	fi, _ := os.Stat(p)
	if respKV(out, "bytes") != strconv.FormatInt(fi.Size(), 10) {
		t.Errorf("bytes=%s, file has %d", respKV(out, "bytes"), fi.Size())
	}
	if respKV(out, "lines") != strconv.Itoa(len(wantLines(fx.recs, nil))) || respKV(out, "matched") != "300" || respKV(out, "exact") != "1" {
		t.Errorf("counts in %s", out)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// body lines never start with '#'
	for _, l := range bodyLines(content) {
		if l == "" || l[0] < '0' || l[0] > '9' {
			t.Fatalf("bad body line %q", l)
		}
	}
	// no temp file left
	ents, _ := os.ReadDir(fx.dir)
	if len(ents) != 1 {
		t.Errorf("export dir: %v", ents)
	}
	// a second export in the same second gets a unique name
	p2 := respFile(t, fx.ok(t, `{}`))
	if p2 == p || !strings.HasSuffix(p2, ".txt") {
		t.Errorf("second name %q == %q", p2, p)
	}
}

func TestExportEqualsSearchScanProperty(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 1500)
	rng := rand.New(rand.NewSource(172))
	type fcase struct {
		args string
		keep func(*model.Record) bool
	}
	cases := []fcase{
		{`{}`, func(*model.Record) bool { return true }},
		{`{"filter":"tag=:OkHttp"}`, func(r *model.Record) bool { return r.Tag == "OkHttp" }},
		{`{"tags":["MyApp","chatty"]}`, func(r *model.Record) bool { return r.Tag == "MyApp" || r.Tag == "chatty" }},
		{`{"min_level":"warn"}`, func(r *model.Record) bool { return r.Level.Rank() >= model.Warn.Rank() }},
		{`{"package":"com.ex.a","filter":"level:error"}`, func(r *model.Record) bool { return r.Level.Rank() >= model.Error.Rank() }},
		{`{"filter":"message:payload"}`, func(r *model.Record) bool { return strings.Contains(strings.ToLower(r.Msg), "payload") }}, // not countable
		{`{"filter":"tag=:OkHttp | message:token"}`, func(r *model.Record) bool {
			return r.Tag == "OkHttp" || strings.Contains(strings.ToLower(r.Msg), "token")
		}},
	}
	for _, c := range cases {
		for trial := 0; trial < 4; trial++ {
			lo, hi := 0, len(fx.recs)
			args := c.args
			if trial > 0 { // window and cursor
				lo = rng.Intn(len(fx.recs) / 2)
				hi = lo + 50 + rng.Intn(len(fx.recs)-lo-50)
				from := time.Unix(0, fx.recs[lo].TimeNs).UTC().Format("2006-01-02T15:04:05.000Z")
				to := time.Unix(0, fx.recs[hi].TimeNs).UTC().Format("2006-01-02T15:04:05.000Z")
				args = strings.TrimSuffix(c.args, "}")
				if args != "{" {
					args += ","
				}
				args += fmt.Sprintf(`"from":%q,"to":%q}`, from, to)
			}
			var want []*model.Record
			for _, r := range fx.recs[lo:min(hi, len(fx.recs))] {
				if c.keep(r) && r.TimeNs >= fx.recs[lo].TimeNs && (trial == 0 || r.TimeNs < fx.recs[hi].TimeNs) {
					want = append(want, r)
				}
			}
			out, isErr := fx.run(args)
			if len(want) == 0 {
				if !strings.HasPrefix(out, "empty view=export") {
					t.Fatalf("%s: want empty:\n%s", args, out)
				}
				continue
			}
			if isErr {
				t.Fatalf("%s: %s", args, out)
			}
			got := bodyLines(readFile(t, respFile(t, out)))
			eqLines(t, args, got, wantLines(want, nil))
			if respKV(out, "matched") != strconv.Itoa(len(want)) {
				t.Errorf("%s: matched %s, want %d", args, respKV(out, "matched"), len(want))
			}
			// the same selection through logcat_search pages matches the file
			if trial == 0 && c.args != `{}` {
				s, _ := fx.tools.Call(context.Background(), "logcat_search", json.RawMessage(strings.TrimSuffix(c.args, "}")+`,"limit":1}`))
				if want := respKV(s, "matched"); want != "" && want != respKV(out, "matched") {
					t.Errorf("%s: search matched %s, export %s", c.args, want, respKV(out, "matched"))
				}
			}
		}
	}
	// after cursor: exclusive; before: up to the line
	cur := fx.tools.cursor(&query{d: mustDev(t, fx)}, fx.recs[999].Seq)
	out := fx.ok(t, fmt.Sprintf(`{"after":%q}`, cur))
	eqLines(t, "after", bodyLines(readFile(t, respFile(t, out))), wantLines(fx.recs[1000:], nil))
	cur = fx.tools.cursor(&query{d: mustDev(t, fx)}, fx.recs[100].Seq)
	out = fx.ok(t, fmt.Sprintf(`{"before":%q}`, cur))
	eqLines(t, "before", bodyLines(readFile(t, respFile(t, out))), wantLines(fx.recs[:100], nil))
}

func mustDev(t *testing.T, fx *exFx) *device.Device {
	d := fx.tools.M.Get("emu-x")
	if d == nil {
		t.Fatal("device emu-x not loaded")
	}
	return d
}

func TestExportClockJumpMarkers(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 400, 150, 300)
	out := fx.ok(t, `{}`)
	content := readFile(t, respFile(t, out))
	if respKV(out, "clock_jumps") != "2" || !strings.Contains(content, "# clock_jumps=2") {
		t.Errorf("clock_jumps: %s", out)
	}
	// the marker comes right before the line at the jump's seq, in arrival order
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var marks []int
	for i, l := range lines {
		if strings.HasPrefix(l, "--------- clock jump seq=") {
			marks = append(marks, i)
			if !strings.Contains(l, "source=live") || !strings.Contains(l, "delta_s=-3599.0") {
				t.Errorf("marker %q", l)
			}
		}
	}
	if len(marks) != 2 {
		t.Fatalf("markers: %v", marks)
	}
	for k, at := range []int{150, 300} {
		next := lines[marks[k]+1]
		want := wantLines(fx.recs[at:at+1], nil)[0]
		if next != want {
			t.Errorf("after marker %d: %q, want %q", k, next, want)
		}
	}
	// a jump whose own line is filtered out is still marked before the next written line
	var pick string
	for i := 150; i < 160; i++ {
		if fx.recs[i].Tag != exTags[150%len(exTags)] {
			pick = fx.recs[i].Tag
			break
		}
	}
	out = fx.ok(t, fmt.Sprintf(`{"filter":"tag=:%s"}`, pick))
	c2 := readFile(t, respFile(t, out))
	if !strings.Contains(c2, "--------- clock jump seq="+strconv.FormatUint(fx.recs[150].Seq, 10)) {
		t.Errorf("marker of a filtered-out jump line missing:\n%s", c2[:min(len(c2), 800)])
	}
	if respKV(out, "clock_jumps") != "2" {
		t.Errorf("%s", out)
	}
}

func TestExportCapsAndContinuation(t *testing.T) {
	// not parallel: changes package variables
	fx := newExFx(t, 14000)
	// 14000 lines of about 100 bytes are more than 1 MiB
	var all []string
	args := `{"max_mb":1}`
	parts := 0
	for {
		out, isErr := fx.run(args)
		if isErr {
			t.Fatalf("%s", out)
		}
		parts++
		p := respFile(t, out)
		content := readFile(t, p)
		if fi, _ := os.Stat(p); fi.Size() > 1<<20+4096 {
			t.Errorf("part %d is %d bytes, over max_mb=1 by more than a line", parts, fi.Size())
		}
		all = append(all, bodyLines(content)...)
		if !strings.Contains(out, "truncated newer=") {
			if strings.Contains(out, "INCOMPLETE") {
				t.Errorf("complete export says incomplete:\n%s", out)
			}
			break
		}
		if !strings.Contains(out, "cap=max_mb") || !strings.Contains(out, "next: after=") || !strings.Contains(out, "INCOMPLETE") || !strings.Contains(content, "truncated=max_mb") {
			t.Fatalf("truncation not explained:\n%s", out)
		}
		m := regexp.MustCompile(`next: after="([^"]+)"`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no cursor:\n%s", out)
		}
		args = fmt.Sprintf(`{"max_mb":1,"after":%q}`, m[1])
		if parts > 20 {
			t.Fatal("too many parts")
		}
	}
	if parts < 2 {
		t.Fatalf("expected a truncated export, got %d part", parts)
	}
	eqLines(t, "part1+part2+... == one export", all, wantLines(fx.recs, nil))
}

func TestExportTimeBudget(t *testing.T) {
	fx := newExFx(t, 3000)
	oldB, oldS := exportBudget, exportSince
	t.Cleanup(func() { exportBudget, exportSince = oldB, oldS })
	exportBudget = 20 * time.Second
	exportSince = func(time.Time) time.Duration { return 21 * time.Second } // the budget is over at the first check
	out := fx.ok(t, `{"redact":true}`)
	if !strings.Contains(out, "cap=time_budget") || !strings.Contains(out, "scan_stopped_ms=20000") || !strings.Contains(out, "next: after=") || !strings.Contains(out, "INCOMPLETE") || !strings.Contains(out, "20 s time budget") {
		t.Fatalf("time budget truncation not explained:\n%s", out)
	}
	if strings.Contains(out, "max_mb=") && strings.Contains(out, "| max_mb") {
		t.Errorf("max_mb is no remedy for the time budget:\n%s", out)
	}
	content := readFile(t, respFile(t, out))
	if !strings.Contains(content, "# end ") || !strings.Contains(content, "truncated=time_budget") {
		t.Errorf("footer missing:\n%s", content[max(0, len(content)-300):])
	}
	// continue until done: the pieces make the whole
	exportSince = oldS
	var got []string
	got = append(got, bodyLines(content)...)
	m := regexp.MustCompile(`next: after="([^"]+)"`).FindStringSubmatch(out)
	out2 := fx.ok(t, fmt.Sprintf(`{"redact":true,"after":%q}`, m[1]))
	got = append(got, bodyLines(readFile(t, respFile(t, out2)))...)
	// redacted messages differ in content, so only the line count is compared
	if want := len(wantLines(fx.recs, nil)); len(got) != want {
		t.Errorf("pieces have %d lines, whole %d", len(got), want)
	}
}

func TestExportRedaction(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 60)
	raw := fx.ok(t, `{}`)
	rawContent := readFile(t, respFile(t, raw))
	if !strings.Contains(rawContent, "alice4@example.com") || respKV(raw, "redacted") != "0" {
		t.Fatalf("raw export must keep the message as recorded:\n%s", raw)
	}
	out := fx.ok(t, `{"redact":true,"redact_extra":["payload (\\d+)"]}`)
	p := respFile(t, out)
	if strings.Contains(filepath.Base(p), "emu-x") || !regexp.MustCompile(`^export-dev-[0-9a-f]{8}-\d{8}-\d{6}\.txt$`).MatchString(filepath.Base(p)) {
		t.Errorf("device id in the file name of a redacted export: %s", p)
	}
	if strings.Contains(out, "note code=unredacted") || respKV(out, "redacted") != "1" || respKV(out, "redaction_hits") == "" || respKV(out, "redaction_hits") == "0" {
		t.Errorf("response:\n%s", out)
	}
	c := readFile(t, p)
	for _, secret := range []string{"alice4@example.com", "abcdef0123456789abcdef4"} {
		if strings.Contains(c, secret) {
			t.Errorf("%q survived redaction", secret)
		}
	}
	if !strings.Contains(c, "[REDACTED:email#") || !strings.Contains(c, "# redacted=1 rules=builtin extra_rules=1") || strings.Contains(c, "# WARNING") || !strings.Contains(c, "# note: pattern-based redaction, not complete") {
		t.Errorf("header of a redacted export:\n%s", c[:min(len(c), 1200)])
	}
	// tag, pid and time are kept
	b := bodyLines(c)
	if len(b) != len(bodyLines(rawContent)) {
		t.Errorf("redaction changed the number of lines: %d vs %d", len(b), len(bodyLines(rawContent)))
	}
	for i := range b {
		if b[i][:strings.Index(b[i], ": ")] != bodyLines(rawContent)[i][:strings.Index(bodyLines(rawContent)[i], ": ")] {
			t.Fatalf("line prefix changed: %q", b[i])
		}
	}
	// a secret in the filter is redacted in the header
	out = fx.ok(t, `{"redact":true,"filter":"message:alice4@example.com | tag=:OkHttp"}`)
	if strings.Contains(readFile(t, respFile(t, out)), "alice4@example.com") {
		t.Errorf("filter text not redacted in the header")
	}
	// errors
	for _, c := range []struct{ args, code string }{
		{`{"redact_extra":["x"]}`, "bad_param"},
		{`{"redact":true,"redact_extra":["a","b","c","d","e","f","g","h","i"]}`, "bad_param"},
		{`{"redact":true,"redact_extra":["("]}`, "bad_param"},
		{`{"max_mb":0,"x":1}`, "unknown_param"},
		{`{"max_mb":513}`, "bad_param"},
		{`{"max_mb":-1}`, "bad_param"},
		{`{"filter":"tag:("}`, "bad_filter"},
		{`{"from":"yesterday-ish"}`, "bad_time"},
		{`{"after":"nonsense"}`, "bad_cursor"},
		{`{"from":"host:1h"}`, "host_time"},
	} {
		out, isErr := fx.run(c.args)
		if !isErr || !strings.HasPrefix(out, "error code="+c.code) {
			t.Errorf("%s: want error code=%s, got %v %q", c.args, c.code, isErr, out)
		}
	}
	if ents, _ := os.ReadDir(fx.dir); len(ents) != 3 {
		t.Errorf("errors must not leave files: %v", ents)
	}
}

func TestExportEmptyWritesNoFile(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 50)
	out, isErr := fx.run(`{"filter":"tag=:NoSuchTag"}`)
	if isErr || !strings.HasPrefix(out, "empty view=export") || !strings.Contains(out, "matched=0") || !strings.Contains(out, "hint code=") {
		t.Fatalf("%v %s", isErr, out)
	}
	if ents, _ := os.ReadDir(fx.dir); len(ents) != 0 {
		t.Errorf("an empty export left %v", ents)
	}
}

func TestExportWriteFailureLeavesNoFile(t *testing.T) {
	fx := newExFx(t, 2000)
	t.Cleanup(func() { exportWrap = nil })
	exportWrap = func(w io.Writer) io.Writer { return &failAfter{w: w, left: 20000} }
	out, isErr := fx.run(`{}`)
	if !isErr || !strings.HasPrefix(out, "error code=export_failed") || !strings.Contains(out, "next: android-log-mcp service status") {
		t.Fatalf("%v %s", isErr, out)
	}
	if ents, _ := os.ReadDir(fx.dir); len(ents) != 0 {
		t.Errorf("failed export left %v", ents)
	}
}

type failAfter struct {
	w    io.Writer
	left int
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, errors.New("no space left on device")
	}
	f.left -= len(p)
	return f.w.Write(p)
}

func TestExportCancelLeavesNoFile(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 3000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, isErr := fx.tools.Call(ctx, "logcat_export", json.RawMessage(`{}`))
	if !isErr || !strings.Contains(out, "canceled") {
		t.Fatalf("%v %s", isErr, out)
	}
	if ents, _ := os.ReadDir(fx.dir); len(ents) != 0 {
		t.Errorf("canceled export left %v", ents)
	}
}

func TestExportDiskLow(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 50)
	fx.tools.M.Disk = &store.DiskGuard{MinFree: 512 << 20, Free: func(string) (uint64, error) { return 560 << 20, nil }}
	out, isErr := fx.run(`{"max_mb":64}`)
	if !isErr || !strings.HasPrefix(out, "error code=disk_low free_mb=560") || !strings.Contains(out, "next: max_mb=") {
		t.Fatalf("%v %s", isErr, out)
	}
	fx.tools.M.Disk = &store.DiskGuard{MinFree: 512 << 20, Free: func(string) (uint64, error) { return 5 << 30, nil }}
	fx.ok(t, `{"max_mb":64}`)
}

func TestExportNeedsExportRoot(t *testing.T) {
	t.Parallel()
	fx := newExFx(t, 20)
	fx.tools.ExportRoot = layout.Root{}
	out, isErr := fx.run(`{}`)
	if !isErr || !strings.HasPrefix(out, "error code=export_failed") {
		t.Fatalf("%v %s", isErr, out)
	}
}

func TestExportLogLineFormat(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("x", 8*3600)
	ts := time.Date(2026, 10, 17, 9, 12, 3, 456_000_000, loc).UnixNano()
	b, _, n := appendLogLines(nil, nil, &model.Record{TimeNs: ts, PID: 1234, TID: 1250, Level: model.Error, Tag: "OkHttp", Msg: "hello"}, loc, "hello")
	if string(b) != "2026-10-17 09:12:03.456  1234  1250 E OkHttp  : hello\n" || n != 1 {
		t.Errorf("%q", b)
	}
	b, _, n = appendLogLines(nil, nil, &model.Record{TimeNs: ts, PID: 7, TID: 8, Level: model.Fatal, Tag: ""}, loc, "a\r\nb\n\nc")
	want := "2026-10-17 09:12:03.456     7     8 F         : a\n2026-10-17 09:12:03.456     7     8 F         : b\n2026-10-17 09:12:03.456     7     8 F         : \n2026-10-17 09:12:03.456     7     8 F         : c\n"
	if string(b) != want || n != 4 {
		t.Errorf("%q", b)
	}
	b, _, _ = appendLogLines(nil, nil, &model.Record{TimeNs: ts, PID: 1, TID: 1, Tag: "LongTagNameOver8"}, loc, "")
	if string(b) != "2026-10-17 09:12:03.456     1     1 I LongTagNameOver8: \n" {
		t.Errorf("%q", b)
	}
}

func TestToolAnnotations(t *testing.T) {
	t.Parallel()
	for _, d := range ToolDefs() {
		a := toolAnnotations(d)
		switch d.Name {
		case "logcat_export":
			if a.ReadOnlyHint || a.IdempotentHint || !d.Writes || !d.NotIdempotent {
				t.Errorf("logcat_export must be non-read-only, non-idempotent: %+v", a)
			}
			if a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Errorf("logcat_export: not destructive, not open world: %+v", a)
			}
		case "logcat_status": // starts recording, record_for pins it
			if a.ReadOnlyHint || !a.IdempotentHint {
				t.Errorf("logcat_status: %+v", a)
			}
		default:
			if !a.ReadOnlyHint || !a.IdempotentHint {
				t.Errorf("%s must stay read-only: %+v", d.Name, a)
			}
		}
	}
	// the definition travels as JSON: an older reader ignores the new fields
	b, _ := json.Marshal(ToolDefs()[0])
	if !strings.Contains(string(b), `"writes":true`) {
		t.Errorf("%s", b)
	}
}

// logcat_status lists the newest export file: name, size, age and whether it
// is complete; nothing without one.
func TestStatusListsNewestExport(t *testing.T) {
	// not parallel: changes package variables
	fx := newExFx(t, 14000)
	st, _ := fx.tools.Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"emu-x"}`))
	if strings.Contains(st, "\n  export ") {
		t.Fatalf("no export yet:\n%s", st)
	}
	out := fx.ok(t, `{"max_mb":1}`) // truncated: incomplete
	p := respFile(t, out)
	st, _ = fx.tools.Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"emu-x"}`))
	want := "  " + hdr("export", "file", p) + " bytes=" // quoted the way the response quotes it (Windows paths)
	if !strings.Contains(st, want) || !strings.Contains(st, " complete=0\n") || !strings.Contains(st, " age_s=") {
		t.Fatalf("status lacks %q complete=0:\n%s", want, st)
	}
	fx.ok(t, `{"after":"`+regexp.MustCompile(`next: after="([^"]+)"`).FindStringSubmatch(out)[1]+`","max_mb":64}`) // the rest: complete
	st, _ = fx.tools.Call(context.Background(), "logcat_status", json.RawMessage(`{"device":"emu-x"}`))
	if strings.Count(st, "\n  export ") != 1 || !strings.Contains(st, " complete=1\n") {
		t.Fatalf("newest file is complete:\n%s", st)
	}
}

// `export --out`: the temp file is created in <root>/export/, never in the
// user's directory, and is renamed into place (issue #206).
func TestExportOutTempFileLivesInExportDir(t *testing.T) {
	// not parallel: changes a package variable
	fx := newExFx(t, 50)
	userDir := t.TempDir()
	out := filepath.Join(userDir, "log.txt")
	tmps := func(d string) int {
		m, _ := filepath.Glob(filepath.Join(d, ".export-*.tmp"))
		return len(m)
	}
	var inUser, inExport int
	exportWrap = func(w io.Writer) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			inUser, inExport = max(inUser, tmps(userDir)), max(inExport, tmps(fx.dir))
			return w.Write(p)
		})
	}
	defer func() { exportWrap = nil }()
	text, isErr := fx.tools.ExportToFile(context.Background(), json.RawMessage(`{}`), ExportTarget{Out: out})
	if isErr || !strings.HasPrefix(text, "ok view=export") {
		t.Fatalf("%s", text)
	}
	if inUser != 0 || inExport != 1 {
		t.Errorf("temp files while writing: user dir %d, export dir %d (want 0 and 1)", inUser, inExport)
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), "# end ") {
		t.Fatalf("file: %v", err)
	}
	if tmps(userDir) != 0 || tmps(fx.dir) != 0 {
		t.Error("temp file left behind")
	}
	if ents, _ := os.ReadDir(userDir); len(ents) != 1 {
		t.Errorf("user dir: %v", ents)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
