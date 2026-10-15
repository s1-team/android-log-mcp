package mcpserver

// Output-format contract of #85 for logcat_crashes, logcat_app_launches and
// logcat_status: first token, fmt=1, ISO times, one-token process names,
// executable next: clauses. The tests read tool output only.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func applyCalls() []struct{ tool, args, word string } {
	return []struct{ tool, args, word string }{
		{"logcat_crashes", `{}`, "ok"},
		{"logcat_crashes", `{"id":"cr1"}`, "ok"},
		{"logcat_crashes", `{"id":"cr1","before_lines":3}`, "ok"},
		{"logcat_crashes", `{"package":"com.nope.app"}`, "empty"},
		{"logcat_crashes", `{"id":"cr99"}`, "error"},
		{"logcat_crashes", `{"before_lines":500,"id":"cr1"}`, "error"},
		{"logcat_crashes", `{"kinds":["oom"]}`, "error"},
		{"logcat_app_launches", `{"package":"com.example.logtest"}`, "ok"},
		{"logcat_app_launches", `{"package":"com.nope.app"}`, "empty"},
		{"logcat_app_launches", `{"limit":99}`, "error"},
		{"logcat_status", `{}`, "ok"},
		{"logcat_status", `{"package":"com.example.logtest"}`, "ok"},
		{"logcat_status", `{"verbose":true}`, "ok"},
		{"logcat_status", `{"device":"nope"}`, "error"},
		{"logcat_status", `{"record_for":"25h"}`, "error"},
	}
}

func TestFormatApplyFirstTokenAndFmt(t *testing.T) {
	e := contractEnv(t)
	for _, c := range applyCalls() {
		out, _ := e.call(c.tool, c.args)
		h := firstLine(out)
		if f := strings.Fields(h); len(f) == 0 || f[0] != c.word {
			t.Errorf("%s %s: first token of %q, want %s", c.tool, c.args, h, c.word)
			continue
		}
		if c.word == "error" {
			if !strings.HasPrefix(h, "error code=") || !strings.Contains(h, " next: ") {
				t.Errorf("%s %s: error without code/next: %s", c.tool, c.args, h)
			}
			continue
		}
		kv := kvOf(h)
		if kv["fmt"] != "1" || kv["dev_now"] == "" && c.tool != "logcat_status" {
			t.Errorf("%s %s: header lacks fmt/dev_now: %s", c.tool, c.args, h)
		}
		if strings.Contains(h, "host_now") && !strings.Contains(c.args, "verbose") {
			t.Errorf("%s %s: host_now in the default header: %s", c.tool, c.args, h)
		}
	}
	out, _ := e.call("logcat_status", `{"verbose":true}`)
	if !strings.Contains(firstLine(out), "host_now=") || !strings.Contains(firstLine(out), "tz=") {
		t.Errorf("verbose status lacks host_now/tz: %s", firstLine(out))
	}
}

// Times are ISO-8601 with an offset, numbers are plain integers (no
// thousands separators) and the structure is ASCII.
func TestFormatApplyStructure(t *testing.T) {
	e := contractEnv(t)
	oldTime := regexp.MustCompile(`\b\d\d-\d\d \d\d:\d\d|\d{4}-\d\d-\d\d \d\d:\d\d:\d\d|host time|device time|\d,\d{3}\b`)
	for _, c := range applyCalls() {
		out, _ := e.call(c.tool, c.args)
		for _, l := range strings.Split(out, "\n") {
			ind := strings.TrimLeft(l, " ")
			// log message rows, stack/tombstone bodies and trace text are raw log content
			if regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3} `).MatchString(ind) || strings.HasPrefix(ind, "at ") || strings.HasPrefix(ind, "|") ||
				strings.HasPrefix(ind, "Caused by") || strings.HasPrefix(ind, "\"") || strings.HasPrefix(ind, "#") && !strings.Contains(ind, " launch ") {
				continue
			}
			first := strings.Fields(ind + " x")[0]
			structural := map[string]bool{"ok": true, "empty": true, "error": true, "crash": true, "step": true, "ttid": true, "ttfd": true, "proc": true,
				"device": true, "recording": true, "window": true, "stored": true, "memory": true, "package": true, "hint": true, "note": true,
				"truncated": true, "adb": true, "store": true, "mode": true, "same_signature": true, "clock_jumps": true, "pin": true, "pinned": true}
			if !structural[first] && !strings.HasPrefix(first, "#") {
				continue
			}
			if oldTime.MatchString(l) {
				t.Errorf("%s %s: prose time or separator in %q", c.tool, c.args, l)
			}
			for _, r := range l {
				if r > 126 {
					t.Errorf("%s %s: non-ASCII %q in %q", c.tool, c.args, r, l)
					break
				}
			}
		}
	}
}

// Every next: of a crashes/launches/status response names real parameters.
func TestFormatApplyNextIsExecutable(t *testing.T) {
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
	for _, c := range applyCalls() {
		out, _ := e.call(c.tool, c.args)
		for _, l := range strings.Split(out, "\n") {
			_, next, ok := strings.Cut(l, " next: ")
			if !ok || strings.HasPrefix(strings.TrimSpace(l), "|") {
				continue
			}
			if strings.Contains(next, "<") {
				t.Errorf("placeholder in next: %q", l)
			}
			for _, alt := range strings.Split(next, " | ") {
				tl := c.tool
				alt = strings.TrimSpace(alt)
				if strings.HasPrefix(alt, "logcat_") {
					tl, alt, _ = strings.Cut(alt, " ")
				}
				if !strings.Contains(alt, "=") || strings.HasPrefix(alt, "adb ") || strings.HasPrefix(alt, "accept ") {
					continue
				}
				for _, m := range paramRe.FindAllStringSubmatch(" "+alt, -1) {
					if !schema[tl][m[1]] {
						t.Errorf("%s %s: next names %q, not a parameter of %s: %q", c.tool, c.args, m[1], tl, l)
					}
				}
			}
		}
	}
}

// The crash detail shows 20 lines before the crash by default (B7), cut with
// a truncated line that names the larger value; list entries have no
// process column for the lines before the crash.
func TestFormatCrashDetailBeforeLines(t *testing.T) {
	e := contractEnv(t)
	out, _ := e.call("logcat_crashes", `{"id":"cr1","before_lines":3}`)
	if !strings.Contains(out, "truncated before_lines=3 exact=0 next: before_lines=6") {
		t.Errorf("before_lines=3 not cut with a next:\n%s", out)
	}
	n := 0
	in := false
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "before_crash ") {
			in = true
			continue
		}
		if in && strings.HasPrefix(l, "  ") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("before_crash lines = %d, want 3:\n%s", n, out)
	}
}

// Process names in status rows are one token, ended processes say why.
func TestFormatStatusProcRows(t *testing.T) {
	e := contractEnv(t)
	out, _ := e.call("logcat_status", `{"package":"com.example.logtest"}`)
	if !strings.Contains(out, "\npackage id=com.example.logtest installed=") {
		t.Fatalf("no package line:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "proc ") && !regexp.MustCompile(`^\s+proc pid=\d+ name=\S+ state=(running|ended) `).MatchString(l) {
			t.Errorf("proc row: %q", l)
		}
	}
}
