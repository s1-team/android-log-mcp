package mcpserver

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// frames returns n frame lines "\tat <pkg>.f<i>(F.java:<i>)".
func frames(pkg string, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("\tat %s.f%d(F.java:%d)", pkg, i, 100+i))
	}
	return out
}

func stackMsg(parts ...[]string) string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return strings.Join(all, "\n")
}

func one(s string) []string { return []string{s} }

func TestShortStackCausedByChain(t *testing.T) {
	t.Parallel()
	msg := stackMsg(one("request failed"), one("java.lang.RuntimeException: wrapper"), frames("a.Top", 25),
		one("Caused by: java.io.IOException: middle"), frames("b.Mid", 12), one("\t... 20 more"),
		one("Caused by: java.net.SocketTimeoutException: root cause here"), frames("c.Root", 9), one("\t... 30 more"))
	out, hidden, changed := shortStack(msg)
	want := stackMsg(one("request failed"), one("java.lang.RuntimeException: wrapper"), frames("a.Top", 3), one("\t^ frames_hidden=22"),
		one("Caused by: java.io.IOException: middle"), frames("b.Mid", 3), one("\t^ frames_hidden=29"),
		one("Caused by: java.net.SocketTimeoutException: root cause here"), frames("c.Root", 3), one("\t^ frames_hidden=36"))
	if out != want || !changed || hidden != 22+29+36 {
		t.Fatalf("hidden %d changed %v:\n%s\nwant:\n%s", hidden, changed, out, want)
	}
}

func TestShortStackSuppressedAndMore(t *testing.T) {
	t.Parallel()
	msg := stackMsg(one("java.lang.IllegalStateException: close failed"), frames("a.Main", 5),
		one("\tSuppressed: java.io.IOException: flush"), []string{"\t\tat x.Flush.a(F.java:1)", "\t\tat x.Flush.b(F.java:2)", "\t\t... 5 more"},
		one("Caused by: java.lang.Error: root"), frames("c.Root", 2), one("\t... 4 more"))
	out, hidden, _ := shortStack(msg)
	want := stackMsg(one("java.lang.IllegalStateException: close failed"), frames("a.Main", 3), one("\t^ frames_hidden=2"),
		one("\tSuppressed: java.io.IOException: flush"), one("\t\t^ frames_hidden=7"),
		one("Caused by: java.lang.Error: root"), frames("c.Root", 2), one("\t^ frames_hidden=4"))
	if out != want || hidden != 2+7+4 {
		t.Fatalf("hidden %d:\n%s\nwant:\n%s", hidden, out, want)
	}
}

func TestShortStackUnchangedWhenShort(t *testing.T) {
	t.Parallel()
	msg := stackMsg(one("java.lang.Error: x"), frames("a", 3), one("Caused by: java.lang.Error: y"), frames("b", 1))
	if out, hidden, changed := shortStack(msg); out != msg || hidden != 0 || changed {
		t.Fatalf("changed a short stack:\n%s", out)
	}
	// 4-space "at" (some loggers) is a frame too
	msg = "java.lang.Error: x\n    at a.b(C.java:1)\n    at a.c(C.java:2)\n    at a.d(C.java:3)\n    at a.e(C.java:4)"
	if out, _, _ := shortStack(msg); out != "java.lang.Error: x\n    at a.b(C.java:1)\n    at a.c(C.java:2)\n    at a.d(C.java:3)\n    ^ frames_hidden=1" {
		t.Fatalf("4-space frames:\n%s", out)
	}
}

// Over 2,000 chars the causes win: every Caused by head and its first
// frame stay, the last (root) cause included; the rest gives way.
func TestShortStackCapKeepsCauses(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 150)
	var parts [][]string
	parts = append(parts, one("java.lang.RuntimeException: "+strings.Repeat("top message ", 200)))
	parts = append(parts, frames("a.Top."+long, 10))
	for i := 0; i < 6; i++ {
		parts = append(parts, one(fmt.Sprintf("Caused by: java.lang.Exception: cause %d %s", i, strings.Repeat("m", 400))), frames(fmt.Sprintf("c%d.%s", i, long[:60]), 10))
	}
	msg := stackMsg(parts...)
	out, _, changed := shortStack(msg)
	if !changed || len(out) > shortStackChars {
		t.Fatalf("len %d (cap %d):\n%s", len(out), shortStackChars, out)
	}
	for i := 0; i < 6; i++ {
		if !strings.Contains(out, fmt.Sprintf("Caused by: java.lang.Exception: cause %d ", i)) || !strings.Contains(out, fmt.Sprintf("at c%d.", i)) {
			t.Fatalf("cause %d or its first frame lost:\n%s", i, out)
		}
	}
	if !strings.Contains(out, `next: stack="full"]`) || !strings.HasPrefix(out, "java.lang.RuntimeException: top message") {
		t.Fatalf("cut marker / head:\n%s", out)
	}
	// fewer frames first: a stack that fits with 1 frame each is not cut
	long = strings.Repeat("x", 250)
	mid := stackMsg(one("java.lang.Error: a"), frames("p."+long, 8), one("Caused by: java.lang.Error: b"), frames("q."+long, 8), one("Caused by: java.lang.Error: c"), frames("r."+long, 8))
	out, _, _ = shortStack(mid)
	if strings.Contains(out, "chars;") || strings.Count(out, "\tat ") != 3 || !strings.Contains(out, "Caused by: java.lang.Error: c\n\tat r.") {
		t.Fatalf("1 frame each:\n%s", out)
	}
}

func TestPerLineCapHints(t *testing.T) {
	t.Parallel()
	plain := strings.Repeat("y", 2500)
	if got := capMsg(plain); !strings.HasSuffix(got, "...[line_cut=2000 more_chars=500 next: none]") {
		t.Fatalf("plain: %s", got[2000:])
	}
	stack := stackMsg(one("java.lang.Error: x"), frames("a."+strings.Repeat("z", 300), 30))
	if got := capMsg(stack); !strings.Contains(got, "...[line_cut=8000 more_chars=") || !strings.HasSuffix(got, "next: none]") {
		t.Fatalf("stack: %s", got[7900:])
	}
}

// s1riusPattern is an app process that logs a warning with the same
// 30-frame stack and an 8-frame Caused by every second, between 20 debug
// lines (issue #23: ~1 KB per shown line whatever the limit).
func s1riusPattern(n int) string { return s1riusPatternPad(n, "") }

// s1riusPatternPad pads the debug lines (to make context lines heavy).
func s1riusPatternPad(n int, pad string) string {
	var b strings.Builder
	ts := 1791573100.0
	line := func(lvl, msg string) {
		fmt.Fprintf(&b, "         %.6f 10146  7777  7790 %s MyRepo: %s\n", ts, lvl, msg)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < 20; j++ {
			ts += 0.05
			line("D", fmt.Sprintf("load page %d item %d ok in 12 ms%s", i, j, pad))
		}
		ts += 0.05
		line("W", fmt.Sprintf("refresh failed for page %d", i))
		line("W", "java.io.IOException: unexpected end of stream on https://api.example.com/...")
		for f := 0; f < 30; f++ {
			line("W", fmt.Sprintf("\tat okhttp3.internal.http1.Http1ExchangeCodec.readResponseHeaders%d(Http1ExchangeCodec.kt:%d)", f, 200+f))
		}
		line("W", "Caused by: java.io.EOFException: \\n not found: limit=0 content=…")
		for f := 0; f < 8; f++ {
			line("W", fmt.Sprintf("\tat okio.RealBufferedSource.readUtf8LineStrict%d(RealBufferedSource.kt:%d)", f, 330+f))
		}
		line("W", "\t... 22 more")
	}
	return b.String()
}

func TestSearchShortStacksReplay(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot2)
	e.replay(s1riusPattern(10))
	q := `{"filter":"pid:7777 level:warn","limit":10}`
	out := e.ok("logcat_search", q,
		" shown=10 ",
		"\t^ frames_hidden=27\n| Caused by: java.io.EOFException",
		"| Caused by: java.io.EOFException: \\n not found: limit=0 content=…\n| \t^ same_frames=60 as=03:11:41.049\n",
		"\ntruncated stack_frames=594 stacks=10 exact=1 next: stack=\"full\"\n")
	if len(out) > 5000 || strings.Count(out, "same_frames=60 as=03:11:41.049") != 9 {
		t.Fatalf("%d chars:\n%s", len(out), out)
	}
	notContains(t, out, "cap=output")
	dump(t, out)
	// stack="full": every frame, and the 30k cap shows only 7 of the 10
	full := e.ok("logcat_search", `{"filter":"pid:7777 level:warn","limit":10,"stack":"full"}`,
		" shown=7 ", "readResponseHeaders29(", "\t... 22 more",
		" cap=output_30000 next: before=")
	notContains(t, full, "^ same_frames", "stack_frames=")
	if _, isErr := e.call("logcat_search", `{"stack":"brief"}`); !isErr {
		t.Fatal("stack=brief accepted")
	}
}

// The default limit is 50 (#23), and a truncated page points at the summary.
func TestSearchDefaultLimit50(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	out := e.ok("logcat_search", `{}`, " matched=6447 exact=1 shown=50 ",
		"\ntruncated older=6397 exact=1 next: before=")
	notContains(t, out, "cap=output")
}

// With context, the cap drops whole records, so every "same frames as T"
// still points at a line in the response.
func TestSearchSameStackReferencesSurviveCap(t *testing.T) {
	t.Parallel()
	e := newReplay(t, fxBoot2)
	e.replay(s1riusPatternPad(40, strings.Repeat(" .", 200)))
	out := e.ok("logcat_search", `{"filter":"pid:7777 level:warn","limit":40,"context":20}`, "\ntruncated context=")
	refs := regexp.MustCompile(`same_frames=\d+ as=(\d\d:\d\d:\d\d\.\d{3})`).FindAllStringSubmatch(out, -1)
	if len(refs) == 0 {
		t.Fatalf("no references:\n%s", out)
	}
	for _, r := range refs {
		if !strings.Contains(out, "\n"+r[1]+" W 7777/") {
			t.Fatalf("reference to %s, which is not in the response:\n%s", r[1], out)
		}
	}
}

// lineBlock is the row lines of a lines response: everything from the
// first row (or date separator) up to the first truncated / newer: line.
func lineBlock(out string) string {
	var keep []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		if !in && (regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3} `).MatchString(l) || strings.HasPrefix(l, "--- ")) {
			in = true
		}
		if in && (strings.HasPrefix(l, "truncated ") || strings.HasPrefix(l, "newer:")) {
			break
		}
		if in {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// stack="full" shows every record exactly as the current format prints it
// (fmt=1): hashes of lineBlock. They were recorded for #85 (0.8.0), which
// changed the row format (one-token process, "| " continuation lines,
// "^ rep=" markers); before that they pinned the 0.5.2 lines.
func TestSearchStackFullMatchesOldOutput(t *testing.T) {
	t.Parallel()
	gold := []struct{ file, q, sum string }{
		{"session-raw.log", `{"limit":200}`, "1d1a9990b3a57fbe99d39303f2e8094692bad5672c3c4705a328683423f418c6"},
		{"session-raw.log", `{"filter":"level:warn","limit":200}`, "2f04586d03a4ced60fbed008fa42a05d67b5a2a1398c32bd9038ae007fac7a38"},
		{"session-raw.log", `{"filter":"tag:AndroidRuntime","limit":200}`, "e2874bae9aadaad19eca6947702f54447f80a0bf711aff0aa49cddbe96d666b8"},
		{"session-raw.log", `{"filter":"tag:MediaPlayer","limit":30,"context":2}`, "0c4fbe772ee6d63c3c6eddb7c6246d789782bee460bffb1f7dfb372892071dc3"},
		{"session-raw.log", `{"min_level":"error","limit":200}`, "88a6b638300bdefa2a116f251aee507b0ec6cd91285e5a860f111040912956fa"},
		{"session-raw.log", `{"filter":"is:crash","limit":5,"context":3}`, "8b8d26a2518daa43b9ff7c677ee8c548e6e246416f8e7da3d7db3e14c05fdd65"},
		{"session3-raw.log", `{"limit":200}`, "06899e870a2f16a623ec0994eb82a0bd445277539ab0f761c96510bef241c203"},
		{"session3-raw.log", `{"filter":"level:warn","limit":200}`, "bf7de083d7a5cd80d618798c6e69f0e0a333718515cf1d52529438fe524137a5"},
		{"session3-raw.log", `{"filter":"tag:AndroidRuntime","limit":200}`, "e3ea1cbaf0aee3003597af24f6f2c3260f0c24dc6c9fbb601dbd4a24c08bbf2c"},
		{"session3-raw.log", `{"filter":"tag:MediaPlayer","limit":30,"context":2}`, "bc51ce8359bcb476a4ac1ae5375a05083a5ddf2d437696daafd4581657854260"},
		{"session3-raw.log", `{"min_level":"error","limit":200}`, "e364985608603cff61520a256b49f36443a28f50c36c8fd657970dc149af7a96"},
		{"session3-raw.log", `{"filter":"is:crash","limit":5,"context":3}`, "e5da8541477556337b76cbc7453dec94cc223c0639a74c92716611ddac35e665"},
	}
	envs := map[string]*replayEnv{}
	for _, g := range gold {
		e := envs[g.file]
		if e == nil {
			boot := fxBoot1
			if g.file == "session3-raw.log" {
				boot = fxBoot3
			}
			e = newReplay(t, boot)
			e.file(g.file)
			envs[g.file] = e
		}
		q := strings.TrimSuffix(g.q, "}") + `,"stack":"full"}`
		out := e.ok("logcat_search", q)
		if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(lineBlock(out)))); sum != g.sum {
			t.Errorf("%s %s: lines differ from the recorded fmt=1 rows (sha256 %s)", g.file, q, sum)
		}
	}
}
