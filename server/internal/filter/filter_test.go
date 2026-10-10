package filter

import (
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

var prod = Options{}

func rec(tag, msg string) *model.Record {
	return &model.Record{Tag: tag, Msg: msg, Level: model.Warn, PID: 1, TID: 1, UID: -1}
}

func TestProd_sameKeyOR_otherAND(t *testing.T) {
	expectDump(t, "tag:A tag:B", prod, `Or(String("A",TAG), String("B",TAG))`)
	expectDump(t, "-tag:A -tag:B", prod, `And(NegatedString("A",TAG), NegatedString("B",TAG))`)
	expectDump(t, "foo bar", prod, `And(String("foo",IMPLICIT_LINE), String("bar",IMPLICIT_LINE))`)
	expectDump(t, "message:foo message:bar", prod, `Or(String("foo",MESSAGE), String("bar",MESSAGE))`)
	expectDump(t, "level:warn level:error", prod, `Or(Level(WARN), Level(ERROR))`)
	expectDump(t, "age:5m age:1h", prod, `Or(Age(5m), Age(1h))`)
	expectDump(t, "is:crash is:stacktrace", prod, `Or(Crash, StackTrace)`)
	expectDump(t, "tag=:OkHttp tag=:MyRepo level:debug", prod, `And(Or(ExactString("OkHttp",TAG), ExactString("MyRepo",TAG)), Level(DEBUG))`)
	// Juxtaposition binds weaker than |.
	expectDump(t, "tag:a level:error | tag:b", prod, `And(String("a",TAG), Or(Level(ERROR), String("b",TAG)))`)
	// Top-level parens unwrap to their content before grouping (AS toFilter).
	expectDump(t, "(tag:a) tag:b", prod, `Or(String("a",TAG), String("b",TAG))`)
	expectDump(t, "(tag:a | tag:b) tag:c", prod, `And(Or(String("a",TAG), String("b",TAG)), String("c",TAG))`)
	// mixed ops on one key group together
	expectDump(t, "package:com.foo package~:bar$ tag:x", prod, `And(Or(String("com.foo",APP), Regex("bar$",APP)), String("x",TAG))`)
}

func TestProd_notesSameKey(t *testing.T) {
	p, err := Parse("message:foo message:bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "ORed") {
		t.Fatalf("notes: %v", p.Notes)
	}
	if got := p.Interpretation(); got != `message contains "foo" OR message contains "bar"` {
		t.Errorf("interp %q", got)
	}
}

func TestInterpretation(t *testing.T) {
	p, err := Parse(`tag=:OkHttp tag=:MyRepo level:debug -message:heartbeat`)
	if err != nil {
		t.Fatal(err)
	}
	want := `(tag == "OkHttp" OR tag == "MyRepo") AND level >= DEBUG AND NOT message contains "heartbeat"`
	if p.Interpretation() != want {
		t.Errorf("got  %s\nwant %s", p.Interpretation(), want)
	}
}

func TestQuotingAndEscapes(t *testing.T) {
	expectDump(t, `message:"Start proc"`, prod, `String("Start proc",MESSAGE)`)
	expectDump(t, `message:'it\'s'`, prod, `String("it's",MESSAGE)`)
	expectDump(t, `message:"say \"hi\""`, prod, `String("say \"hi\"",MESSAGE)`)
	expectDump(t, `"foo bar"`, prod, `String("foo bar",IMPLICIT_LINE)`)
	expectDump(t, `tag~:"^am_(anr|crash)$"`, prod, `Regex("^am_(anr|crash)$",TAG)`)
	expectDump(t, `message~:"Displayed com\.foo"`, prod, `Regex("Displayed com\\.foo",MESSAGE)`)
	// extension: \: inside unquoted values
	expectDump(t, `message:a\:b`, prod, `String("a:b",MESSAGE)`)
	// value may contain '|' when glued (AS lexing) -> note
	p, err := Parse("tag:foo|bar")
	if err != nil {
		t.Fatal(err)
	}
	if dump(p.Root) != `String("foo|bar",TAG)` || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "tag:foo | tag:bar") {
		t.Errorf("got %s notes %v", dump(p.Root), p.Notes)
	}
	// value with space after colon
	expectDump(t, "tag: foo", prod, `String("foo",TAG)`)
}

func TestExtensions(t *testing.T) {
	expectDump(t, "pid:123", prod, "Num(pid,123,false)")
	expectDump(t, "-pid:123 tid:5", prod, "And(Num(pid,123,true), Num(tid,5,false))")
	expectDump(t, "pid:1 pid:2", prod, "Or(Num(pid,1,false), Num(pid,2,false))")
	expectDump(t, "buffer:events -buffer:main", prod, "And(Buffer(events,false), Buffer(main,true))")
	for _, l := range []string{"E", "e", "error", "ERROR"} {
		expectDump(t, "level:"+l, prod, "Level(ERROR)")
	}
	expectDump(t, "level:F", prod, "Level(ASSERT)")
	expectDump(t, "level:fatal", prod, "Level(ASSERT)")
	// extension: regular-key values stop at ')' so this works
	expectDump(t, "(tag:a | level:error)", prod, `Or(String("a",TAG), Level(ERROR))`)
}

func TestStrictErrors(t *testing.T) {
	e := expectErr(t, "tag:OkHttp lvl:error", prod, `unknown key "lvl"`, "Did you mean: level:error")
	if e.Col() != 12 {
		t.Errorf("col %d want 12", e.Col())
	}
	if !strings.Contains(e.Error(), "\n  tag:OkHttp lvl:error\n             ^") {
		t.Errorf("caret rendering:\n%s", e.Error())
	}
	expectErr(t, "Tag:foo", prod, "col 1", "keys are lowercase", "tag:")
	expectErr(t, "level:err", prod, `level:"err" is not a level`)
	expectErr(t, "age:10min", prod, `age:"10min"`, "age:10m")
	expectErr(t, "(tag:a", prod, "unclosed '('", "(tag:a)")
	expectErr(t, "tag:a)", prod, "unmatched ')'")
	expectErr(t, "(tag:a tag:b)", prod, "inside parentheses terms must be joined with & or |")
	expectErr(t, `message:"abc`, prod, "unterminated")
	expectErr(t, `tag~:"(?<!Ok)Http"`, prod, "look-behind", "RE2")
	expectErr(t, `message~:"(a)\1"`, prod, "backreferences")
	expectErr(t, "tag:a &&  tag:b", prod, `"&&" is not an operator`)
	expectErr(t, "tag:a |", prod, "'|' needs an expression on its right")
	expectErr(t, "| tag:a", prod, "needs an expression on its left")
	expectErr(t, "-level:error", prod, "level: cannot be negated")
	expectErr(t, "http://example.com", prod, `unknown key "http"`, `quote it: "http://example.com"`)
	expectErr(t, "tag:a:b", prod, "unquoted value contains ':'")
	expectErr(t, "tag:", prod, `missing value after "tag:"`)
	expectErr(t, "pid:abc", prod, "not a number")
	expectErr(t, "buffer:radio", prod, "not a recorded buffer")
	expectErr(t, "name:x", prod, "name: is not supported")
	expectErr(t, "package:mine", prod, "package:mine is not supported")
	// quoted text with colon is fine
	expectDump(t, `"http://example.com"`, prod, `String("http://example.com",IMPLICIT_LINE)`)
	expectDump(t, `-package:mine`, prod, `NegatedString("mine",APP)`)
}

// --- evaluator (ported from LogcatFilterTest.kt) ---

func TestEval_lineFormat(t *testing.T) {
	r := &model.Record{TimeNs: 1000 * int64(time.Millisecond), PID: 1, TID: 1, Package: "app1", Tag: "Tag1", Level: model.Warn, Msg: "message1"}
	if got := r.Line(time.UTC); got != "1970-01-01 00:00:01.000 1-1 Tag1 app1 W: message1" {
		t.Errorf("line %q", got)
	}
}

func match(t *testing.T, q string, r *model.Record) bool {
	t.Helper()
	p, err := Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return p.Root.Match(r, &Ctx{Now: time.Unix(0, r.TimeNs), Loc: time.UTC})
}

func TestEval_fields(t *testing.T) {
	m1 := &model.Record{Level: model.Warn, PID: 1, TID: 1, Package: "app1", Process: "app1", Tag: "Tag1", Msg: "message1"}
	cases := []struct {
		q    string
		want bool
	}{
		{"tag:tag1", true}, {"tag:tag2", false},
		{"-tag:tag1", false}, {"-tag:tag2", true},
		{"line~:Tag1.*message", true}, {"line~:Tag2.*message", false},
		{"-line~:Tag1.*message", false}, {"-line~:Tag2.*message", true},
		{"tag=:Tag1", true}, {"tag=:tag1", false}, {"tag=:Tag", false},
		{"-tag=:Tag1", false}, {"-tag=:Tag", true},
		{"package:APP", true}, {"process=:app1", true},
		{"message1", true}, {"MESSAGE1", true}, {`"W:"`, true},
		{`tag~:"(?i)^TAG1$"`, true}, {"tag~:^TAG1$", false},
		{"level:warn", true}, {"level:error", false}, {"level:verbose", true},
		{"pid:1", true}, {"-pid:1", false}, {"tid:2", false},
	}
	for _, c := range cases {
		if got := match(t, c.q, m1); got != c.want {
			t.Errorf("%q => %v want %v", c.q, got, c.want)
		}
	}
}

func TestEval_levelOrdering(t *testing.T) {
	for _, l := range []model.Level{'V', 'D', 'I', 'W', 'E', 'F', 'A'} {
		r := &model.Record{Level: l}
		want := l.Rank() >= model.Info.Rank()
		if got := match(t, "level:info", r); got != want {
			t.Errorf("%c: %v", l, got)
		}
	}
}

func TestEval_age(t *testing.T) {
	p, _ := Parse("age:10s")
	r := &model.Record{TimeNs: 0}
	if !p.Root.Match(r, &Ctx{Now: time.Unix(5, 0)}) || p.Root.Match(r, &Ctx{Now: time.Unix(15, 0)}) {
		t.Error("age")
	}
}

func TestEval_stacktrace(t *testing.T) {
	msg := "Failed metering RPC\n  io.grpc.StatusRuntimeException: UNAVAILABLE\n    at io.grpc.stub.ClientCalls.toStatusRuntimeException(ClientCalls.java:262)\n    at io.grpc.stub.ClientCalls.getUnchecked(ClientCalls.java:243)"
	if !match(t, "is:stacktrace", &model.Record{Level: 'E', Msg: msg}) {
		t.Error("stacktrace E")
	}
	if !match(t, "is:stacktrace", &model.Record{Level: 'V', Msg: msg}) {
		t.Error("stacktrace V")
	}
	if match(t, "is:stacktrace", &model.Record{Level: 'I', Msg: "Not a stacktrace"}) {
		t.Error("not stacktrace")
	}
}

func TestEval_crash(t *testing.T) {
	cases := []struct {
		r    model.Record
		want bool
	}{
		{model.Record{Tag: "AndroidRuntime", Level: 'E', Msg: "FATAL EXCEPTION: main"}, true},
		{model.Record{Tag: "AndroidRuntime", Level: 'E', Msg: "Not a FATAL EXCEPTION"}, false},
		{model.Record{Tag: "AndroidRuntime", Level: 'W', Msg: "FATAL EXCEPTION: main"}, false},
		{model.Record{Tag: "libc", Level: 'F', Msg: "Native crash"}, true},
		{model.Record{Tag: "DEBUG", Level: 'A', Msg: "Native crash"}, true},
		{model.Record{Tag: "libc", Level: 'E', Msg: "Not a native crash"}, false},
		{model.Record{Tag: "DEBUG", Level: 'E', Msg: "Not a native crash"}, false},
	}
	for i, c := range cases {
		if got := match(t, "is:crash", &c.r); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
