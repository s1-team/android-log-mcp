package mcpserver

// Output-format contract of #85 (docs/GOALS.md "Agent-first"): the tests
// assert the text a search call returns, never the helpers behind it.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func contractEnv(t *testing.T) *replayEnv {
	e := newReplay(t, fxBoot1)
	e.file("coldstart.logcat.txt", "java_crash.logcat.txt", "remote.logcat.txt")
	return e
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// kvOf parses "key=value" tokens of a line (quoted values unquoted).
func kvOf(line string) map[string]string {
	m := map[string]string{}
	for _, f := range regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S+)`).FindAllStringSubmatch(line, -1) {
		m[f[1]] = strings.Trim(f[2], `"`)
	}
	return m
}

func TestFormatFirstTokenAndFmt(t *testing.T) {
	e := contractEnv(t)
	for _, c := range []struct{ args, word string }{
		{`{}`, "ok"},
		{`{"view":"summary"}`, "ok"},
		{`{"filter":"message:no-such-text-anywhere"}`, "empty"},
		{`{"view":"summary","filter":"message:no-such-text-anywhere"}`, "empty"},
	} {
		out, isErr := e.call("logcat_search", c.args)
		if isErr {
			t.Fatalf("%s: %s", c.args, out)
		}
		h := firstLine(out)
		if strings.Fields(h)[0] != c.word {
			t.Errorf("%s: first token of %q, want %s", c.args, h, c.word)
		}
		kv := kvOf(h)
		if kv["fmt"] != "1" || kv["device"] != fxSerial || kv["dev_now"] == "" || kv["clock"] == "" {
			t.Errorf("%s: header lacks fmt/device/dev_now/clock: %s", c.args, h)
		}
		if !regexp.MustCompile(`dev_now=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d[+-]\d\d:\d\d `).MatchString(h) {
			t.Errorf("dev_now is not ISO-8601 with offset: %s", h)
		}
		if strings.Contains(h, "host_now") {
			t.Errorf("host now must not be in the default header: %s", h)
		}
	}
	out, _ := e.call("logcat_search", `{"verbose":true}`)
	if !strings.Contains(firstLine(out), "host_now=") || !strings.Contains(out, "\ncolumns: ") {
		t.Errorf("verbose lacks host_now/columns:\n%s", firstLine(out))
	}
}

// The header and structural lines are ASCII, with no thousands separators;
// log message text may carry anything.
func TestFormatStructureIsASCII(t *testing.T) {
	e := contractEnv(t)
	for _, args := range []string{`{}`, `{"limit":3}`, `{"view":"summary"}`, `{"filter":"tag=:LogTest","limit":2}`, `{"filter":"message:zzzz-none"}`} {
		out, _ := e.call("logcat_search", args)
		for _, l := range strings.Split(out, "\n") {
			if regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3} `).MatchString(l) || strings.HasPrefix(l, "| ") || strings.HasPrefix(l, "  ") {
				continue // a row: its message is log text
			}
			for _, r := range l {
				if r > 127 {
					t.Errorf("%s: non-ASCII %q in %q", args, r, l)
					break
				}
			}
			if regexp.MustCompile(`\d,\d{3}`).MatchString(l) {
				t.Errorf("%s: thousands separator in %q", args, l)
			}
		}
	}
}

// P5: the first 5 whitespace-separated tokens of every row are the columns.
func TestFormatRowTokens(t *testing.T) {
	e := contractEnv(t)
	out := e.ok("logcat_search", `{"limit":200}`)
	rowRe := regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3} `)
	rows := 0
	for _, l := range strings.Split(out, "\n") {
		if !rowRe.MatchString(l) {
			continue
		}
		rows++
		f := strings.Fields(l)
		if len(f) < 5 || !regexp.MustCompile(`^[VDIWEF?]$`).MatchString(f[1]) || !regexp.MustCompile(`^\d+/\d+$`).MatchString(f[2]) || !strings.HasSuffix(f[4], ":") {
			t.Fatalf("row does not tokenize as time level pid/tid process tag: %q", l)
		}
		if strings.Contains(f[3], "(") {
			t.Errorf("process column is not one token: %q", l)
		}
	}
	if rows < 20 {
		t.Fatalf("only %d rows", rows)
	}
}

// P2/P3: every truncated / empty-hint / error line carries an executable
// next whose parameters are valid for the tool's own schema.
func TestFormatNextIsExecutable(t *testing.T) {
	e := contractEnv(t)
	schema := map[string]map[string]bool{}
	for _, d := range toolDefs() {
		var s struct{ Properties map[string]json.RawMessage }
		if err := json.Unmarshal([]byte(d.schema), &s); err != nil {
			t.Fatal(err)
		}
		schema[d.name] = map[string]bool{}
		for k := range s.Properties {
			schema[d.name][k] = true
		}
	}
	paramRe := regexp.MustCompile(`(?:^|[ |])(\w+)=`)
	check := func(tool, args, out string) {
		for _, l := range strings.Split(out, "\n") {
			if !(strings.HasPrefix(l, "truncated ") || strings.HasPrefix(l, "hint ") || strings.HasPrefix(l, "error ")) {
				continue
			}
			_, next, ok := strings.Cut(l, " next: ")
			if !ok {
				if strings.HasPrefix(l, "error ") && (strings.Contains(l, "hint=") || strings.Contains(l, "valid=")) {
					continue
				}
				t.Errorf("%s %s: no next in %q", tool, args, l)
				continue
			}
			if strings.Contains(next, "<") {
				t.Errorf("placeholder in next: %q", l)
			}
			for _, alt := range strings.Split(next, " | ") {
				alt = strings.TrimSpace(alt)
				tl := tool
				if strings.HasPrefix(alt, "logcat_") {
					tl, alt, _ = strings.Cut(alt, " ")
					if _, ok := schema[tl]; !ok {
						continue
					}
					alt = strings.TrimSpace(alt) // fall through with the call's params
				}
				if !strings.Contains(alt, "=") {
					continue // a command, not a param
				}
				for _, m := range paramRe.FindAllStringSubmatch(" "+alt, -1) {
					if !schema[tl][m[1]] {
						t.Errorf("%s %s: next names %q, not a parameter of %s: %q", tool, args, m[1], tl, l)
					}
				}
			}
		}
	}
	for _, args := range []string{
		`{"limit":5}`, `{"limit":5,"after":"` + e.cursorNow(t) + `"}`, `{"filter":"message:zzzz-none"}`, `{"package":"com.nope.app"}`,
		`{"tags":["NoSuchTag"]}`, `{"min_level":"loud"}`, `{"filter":"tag:("}`, `{"limit":0,"view":"nope"}`, `{"from":"yesterdayish"}`,
		`{"from":"host:2026-01-01"}`, `{"limit":5,"stack":"short","context":2}`, `{"wait_ms":5}`, `{"before":"x","after":"y"}`, `{"nope":1}`,
		`{"device":"nope"}`,
	} {
		out, _ := e.call("logcat_search", args)
		check("logcat_search", args, out)
	}
	out, _ := e.call("logcat_nope", `{}`)
	check("logcat_nope", "", out)
	if !strings.Contains(out, "logcat_status logcat_search logcat_crashes logcat_app_launches") {
		t.Errorf("unknown tool lists no tools: %s", out)
	}
	if strings.Fields(out)[0] != "error" {
		t.Errorf("error first token: %s", out)
	}
	for _, bad := range []string{`{"min_level":"loud"}`, `{"filter":"tag:("}`} {
		o, isErr := e.call("logcat_search", bad)
		if !isErr || !strings.HasPrefix(o, "error code=") {
			t.Errorf("%s: %q", bad, o)
		}
	}
}

func (e *replayEnv) cursorNow(t *testing.T) string {
	out := e.ok("logcat_search", `{"limit":1}`)
	return kvOf(out[strings.Index(out, "newer: "):])["after"]
}

// #85 summary head: only when matches are hidden, <= 300 chars, tags in
// percent and templates with # for digits; the old tail line is gone.
func TestFormatSummaryHead(t *testing.T) {
	e := contractEnv(t)
	out := e.ok("logcat_search", `{"limit":5}`)
	var head string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "summary ") {
			head = l
		}
	}
	if head == "" {
		t.Fatalf("no summary head:\n%s", out)
	}
	if len(head) > headCap {
		t.Errorf("head is %d chars > %d", len(head), headCap)
	}
	if !regexp.MustCompile(`^summary sampled=\d+ top3_share=~?\d+% tags=\S+:~?(\d+|<1)%`).MatchString(head) {
		t.Errorf("head shape: %q", head)
	}
	if i := strings.Index(head, " msgs="); i > 0 { // templates: every digit run is one #
		for _, m := range regexp.MustCompile(`\d+x"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(head[i:], -1) {
			if regexp.MustCompile(`\d`).MatchString(m[1]) || !strings.Contains(m[1], ": ") {
				t.Errorf("template not normalized (digits left or no tag): %q in %q", m[1], head)
			}
		}
	} else {
		t.Errorf("no msgs in the head: %q", head)
	}
	if strings.Contains(out, "top tags in") {
		t.Errorf("old tail line is back:\n%s", out)
	}
	// nothing hidden: no head
	all := e.ok("logcat_search", `{"filter":"tag=:LogTest","limit":1000}`)
	if kvOf(firstLine(all))["shown"] == kvOf(firstLine(all))["matched"] && strings.Contains(all, "\nsummary ") {
		t.Errorf("head with nothing hidden:\n%s", firstLine(all))
	}
	sum := e.ok("logcat_search", `{"view":"summary"}`)
	if strings.Contains(sum, "\nsummary ") {
		t.Errorf("head in view=summary")
	}
}

func TestFormatTruncatedOlderAndCursor(t *testing.T) {
	e := contractEnv(t)
	out := e.ok("logcat_search", `{"limit":5}`)
	var tr string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "truncated older=") {
			tr = l
		}
	}
	kv := kvOf(tr)
	if tr == "" || kv["exact"] != "1" || kv["before"] == "" {
		t.Fatalf("truncated line: %q in\n%s", tr, out)
	}
	// following the next: reaches older lines
	older := e.ok("logcat_search", `{"limit":5,"before":"`+kv["before"]+`"}`)
	if firstLine(older) == firstLine(out) || !strings.HasPrefix(firstLine(older), "ok ") {
		t.Errorf("before cursor did not page:\n%s", firstLine(older))
	}
}
