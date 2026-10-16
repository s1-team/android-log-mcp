package mcpserver

import (
	"encoding/json"
	"testing"
)

// One quoting rule for header values: bare unless empty or containing a
// space, control character, `"`, `=` or `\`; otherwise a JSON string.
func TestValQuotingRuleWindowsPaths(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{`/home/u/alm`, `/home/u/alm`},
		{`C:/x`, `C:/x`},
		{`C:\x`, `"C:\\x"`},                                  // no space, still quoted: one rule
		{`C:\Program Files\alm`, `"C:\\Program Files\\alm"`}, // space
		{`C:\a,b\c`, `"C:\\a,b\\c"`},                         // comma
		{`\\server\share\root`, `"\\\\server\\share\\root"`}, // UNC
		{``, `""`},
		{`a=b`, `"a=b"`},
		{"line\nbreak", `"line\nbreak"`},
		{`say "hi"`, `"say \"hi\""`},
		{`日本語`, `日本語`},
	} {
		got := val(c.in)
		if got != c.want {
			t.Errorf("val(%q) = %s, want %s", c.in, got, c.want)
		}
		if got[0] == '"' { // a quoted value is a JSON string that reads back as the original
			var back string
			if err := json.Unmarshal([]byte(got), &back); err != nil || back != c.in {
				t.Errorf("val(%q) = %s does not read back (%q, %v)", c.in, got, back, err)
			}
		}
	}
	if got := KV("ok set-root", "root", `C:\Users\me\alm`, "old_root", `C:\Program Files\x`); got != `ok set-root root="C:\\Users\\me\\alm" old_root="C:\\Program Files\\x"` {
		t.Errorf("KV: %s", got)
	}
}
