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
	msg := stackMsg(one("request failed"), one("java.lang.RuntimeException: wrapper"), frames("a.Top", 25),
		one("Caused by: java.io.IOException: middle"), frames("b.Mid", 12), one("\t... 20 more"),
		one("Caused by: java.net.SocketTimeoutException: root cause here"), frames("c.Root", 9), one("\t... 30 more"))
	out, hidden, changed := shortStack(msg)
	want := stackMsg(one("request failed"), one("java.lang.RuntimeException: wrapper"), frames("a.Top", 3), one("\t↳ 22 more frames"),
		one("Caused by: java.io.IOException: middle"), frames("b.Mid", 3), one("\t↳ 29 more frames"),
		one("Caused by: java.net.SocketTimeoutException: root cause here"), frames("c.Root", 3), one("\t↳ 36 more frames"))
	if out != want || !changed || hidden != 22+29+36 {
		t.Fatalf("hidden %d changed %v:\n%s\nwant:\n%s", hidden, changed, out, want)
	}
}

func TestShortStackSuppressedAndMore(t *testing.T) {
	msg := stackMsg(one("java.lang.IllegalStateException: close failed"), frames("a.Main", 5),
		one("\tSuppressed: java.io.IOException: flush"), []string{"\t\tat x.Flush.a(F.java:1)", "\t\tat x.Flush.b(F.java:2)", "\t\t... 5 more"},
		one("Caused by: java.lang.Error: root"), frames("c.Root", 2), one("\t... 4 more"))
	out, hidden, _ := shortStack(msg)
	want := stackMsg(one("java.lang.IllegalStateException: close failed"), frames("a.Main", 3), one("\t↳ 2 more frames"),
		one("\tSuppressed: java.io.IOException: flush"), one("\t\t↳ 7 more frames"),
		one("Caused by: java.lang.Error: root"), frames("c.Root", 2), one("\t↳ 4 more frames"))
	if out != want || hidden != 2+7+4 {
		t.Fatalf("hidden %d:\n%s\nwant:\n%s", hidden, out, want)
	}
}

func TestShortStackUnchangedWhenShort(t *testing.T) {
	msg := stackMsg(one("java.lang.Error: x"), frames("a", 3), one("Caused by: java.lang.Error: y"), frames("b", 1))
	if out, hidden, changed := shortStack(msg); out != msg || hidden != 0 || changed {
		t.Fatalf("changed a short stack:\n%s", out)
	}
	// 4-space "at" (some loggers) is a frame too
	msg = "java.lang.Error: x\n    at a.b(C.java:1)\n    at a.c(C.java:2)\n    at a.d(C.java:3)\n    at a.e(C.java:4)"
	if out, _, _ := shortStack(msg); out != "java.lang.Error: x\n    at a.b(C.java:1)\n    at a.c(C.java:2)\n    at a.d(C.java:3)\n    ↳ 1 more frames" {
		t.Fatalf("4-space frames:\n%s", out)
	}
}

// Over 2,000 chars the causes win: every Caused by head and its first
// frame stay, the last (root) cause included; the rest gives way.
func TestShortStackCapKeepsCauses(t *testing.T) {
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
	if !strings.Contains(out, `chars; stack="full" shows the whole stack]`) || !strings.HasPrefix(out, "java.lang.RuntimeException: top message") {
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
	plain := strings.Repeat("y", 2500)
	if got := capMsg(plain); !strings.HasSuffix(got, "…[+500 chars; lines are cut at 2,000 chars in every query, narrowing does not show more]") {
		t.Fatalf("plain: %s", got[2000:])
	}
	stack := stackMsg(one("java.lang.Error: x"), frames("a."+strings.Repeat("z", 300), 30))
	if got := capMsg(stack); !strings.Contains(got, "chars; stacks are cut at 8,000 chars in every query, narrowing does not show more]") {
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
	e := newReplay(t, fxBoot2)
	e.replay(s1riusPattern(10))
	q := `{"filter":"pid:7777 level:warn","limit":10}`
	out := e.ok("logcat_search", q,
		"showing newest 10 ",
		"\t↳ 27 more frames\n    Caused by: java.io.EOFException",
		"    Caused by: java.io.EOFException: \\n not found: limit=0 content=…\n    \t↳ same 60 frames as 03:11:41.049\n",
		"stacks: 10 shortened (594 frames hidden) · stack=\"full\" shows every frame\n")
	if len(out) > 5000 || strings.Count(out, "same 60 frames as 03:11:41.049") != 9 {
		t.Fatalf("%d chars:\n%s", len(out), out)
	}
	notContains(t, out, "[output cap")
	dump(t, out)
	// stack="full": every frame, and the 30k cap shows only 7 of the 10
	full := e.ok("logcat_search", `{"filter":"pid:7777 level:warn","limit":10,"stack":"full"}`,
		"showing newest 7 ", "readResponseHeaders29(", "\t... 22 more",
		"[output cap: only 7 of the 10 matches asked for fit in ~30k chars · view=\"summary\" gives an overview; or narrow from/to or the filter]\n")
	notContains(t, full, "↳ same", "stacks:")
	if _, isErr := e.call("logcat_search", `{"stack":"brief"}`); !isErr {
		t.Fatal("stack=brief accepted")
	}
}

// The default limit is 50 (#23), and a truncated page points at the summary.
func TestSearchDefaultLimit50(t *testing.T) {
	e := newReplay(t, fxBoot1)
	e.file("session-raw.log")
	out := e.ok("logcat_search", `{}`, "matched 6,447 lines; showing newest 50 ",
		"[truncated: 6,397 older matches not shown · view=\"summary\" shows what the whole range contains]\n")
	notContains(t, out, "[output cap")
}

// With context, the cap drops whole records, so every "same frames as T"
// still points at a line in the response.
func TestSearchSameStackReferencesSurviveCap(t *testing.T) {
	e := newReplay(t, fxBoot2)
	e.replay(s1riusPatternPad(40, strings.Repeat(" .", 200)))
	out := e.ok("logcat_search", `{"filter":"pid:7777 level:warn","limit":40,"context":20}`, "[output cap: context lines cut")
	refs := regexp.MustCompile(`same \d+ frames as (\d\d:\d\d:\d\d\.\d{3})`).FindAllStringSubmatch(out, -1)
	if len(refs) == 0 {
		t.Fatalf("no references:\n%s", out)
	}
	for _, r := range refs {
		if !strings.Contains(out, "\n"+r[1]+" W 7777/") {
			t.Fatalf("reference to %s, which is not in the response:\n%s", r[1], out)
		}
	}
}

var (
	reOldCut = regexp.MustCompile(`…\[\+(\d+) chars; narrow with filter pid:\d+ and from/to around this time to see it whole\]`)
	reNewCut = regexp.MustCompile(`…\[\+([\d,]+) chars; (lines|stacks) are cut at [\d,]+ chars in every query, narrowing does not show more\]`)
)

// lineBlock is the record lines of a lines response, with the cut marker
// normalised (its text changed in #23, the cut did not).
func lineBlock(out string) string {
	_, rest, _ := strings.Cut(out, "format: time level pid/tid process tag: message\n")
	var keep []string
	for _, l := range strings.Split(rest, "\n") {
		if strings.HasPrefix(l, "[") || strings.HasPrefix(l, "more") || strings.HasPrefix(l, "stacks:") {
			break
		}
		keep = append(keep, l)
	}
	s := reOldCut.ReplaceAllString(strings.Join(keep, "\n"), "…[+$1 chars]")
	return reNewCut.ReplaceAllStringFunc(s, func(m string) string {
		return "…[+" + strings.ReplaceAll(reNewCut.FindStringSubmatch(m)[1], ",", "") + " chars]"
	})
}

// stack="full" shows the lines exactly as 0.5.2 did with limit=200: hashes
// of lineBlock taken from main at ba2035f.
func TestSearchStackFullMatchesOldOutput(t *testing.T) {
	gold := []struct{ file, q, sum string }{
		{"session-raw.log", `{"limit":200}`, "b3473278bd597391ceb4721a92bd3ae9533bcebfd59bcc096a860f340ca8c8f2"},
		{"session-raw.log", `{"filter":"level:warn","limit":200}`, "3b185a49df2ef8dd69936f3cd7af50e8c707c3e185ef9f853161a7113584fa6b"},
		{"session-raw.log", `{"filter":"tag:AndroidRuntime","limit":200}`, "6c83407c77a9fdf845076bf740a0529cbdb7e9112c26d6d2b9612209a8e72cf9"},
		{"session-raw.log", `{"filter":"tag:MediaPlayer","limit":30,"context":2}`, "de61b730b5f5fe3feee1792d0533b1bfa85704b6b17080bae7e9c2775f7fc67f"},
		{"session-raw.log", `{"min_level":"error","limit":200}`, "196516b2c4f12e4e320d6957f8640cd786d155305fafcf61be416c78a8f0232d"},
		{"session-raw.log", `{"filter":"is:crash","limit":5,"context":3}`, "383fb5582daf4a996467080dd5c8e1d3bd963016febccc913fd46c8c92012d36"},
		{"session3-raw.log", `{"limit":200}`, "3a203caf0fbe31424b2eea2bc52a8edca8d3fb09f7a2f565e22558f17af462dc"},
		{"session3-raw.log", `{"filter":"level:warn","limit":200}`, "8232e15b4cc8db5b494867e37f61a5ffb13fb81c6043562a0f85cff465a35c91"},
		{"session3-raw.log", `{"filter":"tag:AndroidRuntime","limit":200}`, "4ba1b4d56fe722921a9a74e631427cad625d2efc61a45688ed12a7e398ca493d"},
		{"session3-raw.log", `{"filter":"tag:MediaPlayer","limit":30,"context":2}`, "ef9183d9aaf2925d7721d95c63cec82553096e96e565e72c794166627f374fb4"},
		{"session3-raw.log", `{"min_level":"error","limit":200}`, "4acbd3661ec05457a9856523dea54117955e0d7b94c71a05be736a77ff21e363"},
		{"session3-raw.log", `{"filter":"is:crash","limit":5,"context":3}`, "50f102d91924fcf5d76395b1d21057235c0eb0402677d1dd527d9024b8af1a95"},
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
			t.Errorf("%s %s: lines differ from 0.5.2 (sha256 %s)", g.file, q, sum)
		}
	}
}
