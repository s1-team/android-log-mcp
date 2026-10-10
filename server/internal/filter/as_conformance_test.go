package filter

// Conformance tests ported from Android Studio's
// logcat/testSrc/com/android/tools/idea/logcat/filters/LogcatFilterParserTest.kt
// (AOSP platform/tools/adt/idea, branch studio-main, fetched 2026-10-10).
//
// The AS test helper defaults to joinConsecutiveTopLevelValue=true and
// topLevelSameKeyTreatment=AND; asTest reproduces those knobs.
// Where AS silently falls back to "line contains <whole query>", we assert
// a strict *Error instead (intentional deviation, see design-v3 §3.3).

import (
	"errors"
	"strings"
	"testing"
)

var asTest = Options{JoinConsecutiveValues: true, SameKeyAND: true}

var asKeys = map[string]string{"tag": "TAG", "package": "APP", "message": "MESSAGE", "line": "LINE"}

func mustParse(t *testing.T, q string, o Options) Node {
	t.Helper()
	p, err := ParseWith(q, o)
	if err != nil {
		t.Fatalf("parse %q: unexpected error:\n%v", q, err)
	}
	return p.Root
}

func expectDump(t *testing.T, q string, o Options, want string) {
	t.Helper()
	if got := dump(mustParse(t, q, o)); got != want {
		t.Errorf("parse %q\n got: %s\nwant: %s", q, got, want)
	}
}

func expectErr(t *testing.T, q string, o Options, contains ...string) *Error {
	t.Helper()
	_, err := ParseWith(q, o)
	if err == nil {
		t.Fatalf("parse %q: expected error", q)
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("parse %q: error is %T, want *Error", q, err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("parse %q: error %q does not contain %q", q, err.Error(), c)
		}
	}
	return fe
}

func TestAS_parse_emptyFilter(t *testing.T) {
	if n := mustParse(t, "", asTest); n != nil {
		t.Fatalf("want nil, got %s", dump(n))
	}
}

// AS returns StringFilter(" \t", IMPLICIT_LINE); we treat blank as "no filter".
func TestAS_parse_blankFilter_deviation(t *testing.T) {
	if n := mustParse(t, " \t", asTest); n != nil {
		t.Fatalf("want nil, got %s", dump(n))
	}
}

func TestAS_parse_stringKey(t *testing.T) {
	for k, f := range asKeys {
		expectDump(t, k+": Foo", asTest, `String("Foo",`+f+`)`)
		expectDump(t, k+":Foo", asTest, `String("Foo",`+f+`)`)
	}
}

func TestAS_parse_stringKey_escapeChars(t *testing.T) {
	for k, f := range asKeys {
		q := strings.Join([]string{
			k + `:foo\ bar`,
			k + `:'foobar'`,
			k + `:\'foobar\'`,
			k + `:"foobar"`,
			k + `:\"foobar\"`,
			k + `:foo\\bar`,
		}, "\n")
		want := `And(String("foo bar",F), String("foobar",F), String("'foobar'",F), String("foobar",F), String("\"foobar\"",F), String("foo\\bar",F))`
		expectDump(t, q, asTest, strings.ReplaceAll(want, ",F)", ","+f+")"))
	}
}

func TestAS_parse_negatedStringKey(t *testing.T) {
	for k, f := range asKeys {
		expectDump(t, "-"+k+": Foo", asTest, `NegatedString("Foo",`+f+`)`)
		expectDump(t, "-"+k+":Foo", asTest, `NegatedString("Foo",`+f+`)`)
	}
}

func TestAS_parse_regexKey(t *testing.T) {
	for k, f := range asKeys {
		expectDump(t, k+"~: Foo", asTest, `Regex("Foo",`+f+`)`)
		expectDump(t, k+"~:Foo", asTest, `Regex("Foo",`+f+`)`)
	}
}

func TestAS_parse_negatedRegexKey(t *testing.T) {
	for k, f := range asKeys {
		expectDump(t, "-"+k+"~: Foo", asTest, `NegatedRegex("Foo",`+f+`)`)
		expectDump(t, "-"+k+"~:Foo", asTest, `NegatedRegex("Foo",`+f+`)`)
	}
}

func TestAS_parse_invalidRegex_strict(t *testing.T) {
	expectErr(t, `tag~:\`, asTest, "invalid regex", "col 6")
	expectErr(t, `-tag~:\`, asTest, "invalid regex", "col 7")
}

func TestAS_parse_exactKey(t *testing.T) {
	for k, f := range asKeys {
		expectDump(t, k+"=: Foo", asTest, `ExactString("Foo",`+f+`)`)
		expectDump(t, k+"=:Foo", asTest, `ExactString("Foo",`+f+`)`)
		expectDump(t, "-"+k+"=: Foo", asTest, `NegatedExactString("Foo",`+f+`)`)
		expectDump(t, "-"+k+"=:Foo", asTest, `NegatedExactString("Foo",`+f+`)`)
	}
}

func TestAS_parse_levelKeys(t *testing.T) {
	for _, l := range []string{"VERBOSE", "DEBUG", "INFO", "WARN", "ERROR", "ASSERT"} {
		expectDump(t, "level: "+l, asTest, "Level("+l+")")
		expectDump(t, "level:"+l, asTest, "Level("+l+")")
		expectDump(t, "level:"+strings.ToLower(l), asTest, "Level("+l+")")
	}
}

func TestAS_parse_levelKeys_invalidLevel_strict(t *testing.T) {
	expectErr(t, "level: Invalid", asTest, `level:"Invalid" is not a level`, "verbose debug info warn error assert")
}

func TestAS_parse_age(t *testing.T) {
	for _, a := range []string{"10s", "10m", "10h", "10d"} {
		expectDump(t, "age: "+a, asTest, "Age("+a+")")
		expectDump(t, "age:"+a, asTest, "Age("+a+")")
	}
}

func TestAS_parse_age_invalid_strict(t *testing.T) {
	for _, a := range []string{"10", "10f", "broom", "99999999999999999999999999999999999999999999999s"} {
		expectErr(t, "age: "+a, asTest, "use <number><s|m|h|d>")
	}
}

func TestAS_isValidLogAge(t *testing.T) {
	for _, a := range []string{"10s", "10m", "10h", "10d"} {
		if _, ok := parseAge(a); !ok {
			t.Errorf("%s should be valid", a)
		}
	}
	for _, a := range []string{"10", "10f", "broom", "99999999999999999999999999999999999999999999999s"} {
		if _, ok := parseAge(a); ok {
			t.Errorf("%s should be invalid", a)
		}
	}
}

func TestAS_isValidLogLevel(t *testing.T) {
	for _, l := range []string{"VERBOSE", "DEBUG", "INFO", "WARN", "ERROR", "ASSERT"} {
		if _, ok := ParseLevel(l); !ok {
			t.Errorf("%s should be valid", l)
		}
	}
	if _, ok := ParseLevel("foo"); ok {
		t.Error("foo should be invalid")
	}
}

func TestAS_parse_isCrash(t *testing.T) { expectDump(t, "is:crash", asTest, "Crash") }

func TestAS_parse_isStacktrace(t *testing.T) { expectDump(t, "is:stacktrace", asTest, "StackTrace") }

func TestAS_parse_isFirebase(t *testing.T) {
	n := mustParse(t, "is:firebase", asTest)
	re, ok := n.(*Regex)
	if !ok || re.Field != FTag {
		t.Fatalf("want tag regex, got %s", dump(n))
	}
	c := &Ctx{}
	if !n.Match(rec("FA", "x"), c) || n.Match(rec("FAQ", "x"), c) {
		t.Error("firebase regex must match FA and not FAQ")
	}
}

// AS (join=true) turns "if:foo" into line contains "if:foo"; production AS
// (join=false) yields line contains "if" AND ":foo". We reject it.
func TestAS_parse_is_invalid_strict(t *testing.T) {
	expectErr(t, "if:foo", asTest, `unknown key "if"`, "Did you mean: is:foo", `quote it: "if:foo"`)
	expectErr(t, "is:foo", asTest, `is:"foo" is not supported`)
}

func TestAS_parse_topLevelExpressions_joinConsecutiveTopLevelValue_true(t *testing.T) {
	q := "level:INFO foo1    bar1   tag:bar2 foo2  package:foobar"
	expectDump(t, q, asTest, `And(Level(INFO), String("foo1    bar1",IMPLICIT_LINE), String("bar2",TAG), String("foo2",IMPLICIT_LINE), String("foobar",APP))`)
}

func TestAS_parse_topLevelExpressions_joinConsecutiveTopLevelValue_false(t *testing.T) {
	q := "level:INFO foo1    bar1   tag:bar2 foo2  package:foobar"
	expectDump(t, q, Options{SameKeyAND: true}, `And(Level(INFO), String("foo1",IMPLICIT_LINE), String("bar1",IMPLICIT_LINE), String("bar2",TAG), String("foo2",IMPLICIT_LINE), String("foobar",APP))`)
}

func TestAS_parse_topLevelExpressions_sameKey_or(t *testing.T) {
	q := "-tag:ignore1 foo tag:tag1 -tag~:ignore2 bar level:WARN tag~:tag2 tag=:tag3 -tag=:ignore3"
	expectDump(t, q, Options{JoinConsecutiveValues: true}, `And(NegatedString("ignore1",TAG), String("foo",IMPLICIT_LINE), Or(String("tag1",TAG), Regex("tag2",TAG), ExactString("tag3",TAG)), NegatedRegex("ignore2",TAG), String("bar",IMPLICIT_LINE), Level(WARN), NegatedExactString("ignore3",TAG))`)
}

func TestAS_parse_and(t *testing.T) {
	expectDump(t, "tag: bar & foo & package: foobar", asTest, `And(String("bar",TAG), String("foo",IMPLICIT_LINE), String("foobar",APP))`)
}

func TestAS_parse_or(t *testing.T) {
	expectDump(t, "tag: bar | foo | package: foobar", asTest, `Or(String("bar",TAG), String("foo",IMPLICIT_LINE), String("foobar",APP))`)
}

func TestAS_parse_operatorPrecedence(t *testing.T) {
	expectDump(t, "f1 & f2 | f3 & f4", asTest, `Or(And(String("f1",IMPLICIT_LINE), String("f2",IMPLICIT_LINE)), And(String("f3",IMPLICIT_LINE), String("f4",IMPLICIT_LINE)))`)
}

func TestAS_parse_parens(t *testing.T) {
	expectDump(t, "f1 & (tag: foo | tag: 'bar') & f4", asTest, `And(String("f1",IMPLICIT_LINE), Or(String("foo",TAG), String("bar",TAG)), String("f4",IMPLICIT_LINE))`)
}

func TestAS_parse_emptyParens(t *testing.T) {
	expectDump(t, "f1 & () & f4", asTest, `And(String("f1",IMPLICIT_LINE), Empty, String("f4",IMPLICIT_LINE))`)
}

// AS: ProjectAppFilter in Android projects, StringFilter("mine",APP) otherwise.
func TestAS_parse_appFilter_strict(t *testing.T) {
	expectErr(t, "package:mine", asTest, "package:mine is not supported", `package="com.foo"`)
}

func TestAS_parse_psiError_strict(t *testing.T) {
	expectErr(t, "key: 'foo", asTest, "unterminated")
}

// AS: NameFilter (always true). Not meaningful without saved filters.
func TestAS_parse_name_strict(t *testing.T) {
	expectErr(t, "level:INFO tag:bar package:foo name:Name", asTest, "name: is not supported")
	expectErr(t, `name:'Name1' name:"Name2"`, asTest, "name: is not supported")
}
