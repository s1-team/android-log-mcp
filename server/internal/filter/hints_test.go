package filter

import (
	"reflect"
	"strings"
	"testing"
)

// Issue #86: syntax-misuse hints. Hints are observable output of Parse; they
// never change the parse tree.
func hintKinds(t *testing.T, q string) ([]Hint, *Parsed) {
	t.Helper()
	p, err := Parse(q)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	return p.Hints, p
}

func TestHints_fire(t *testing.T) {
	for _, c := range []struct{ q, kind, fix string }{
		{"tag:A OR tag:B", HintOrWord, "tag:A | tag:B"},
		{"foo AND bar", HintOrWord, "foo & bar"},
		{"message:rate limit", HintMultiWord, "message:'rate limit'"},
		{"tag=:A -message:rate limit level:error", HintMultiWord, "tag=:A -message:'rate limit' level:error"},
		{"tag:A level:error | tag:B", HintMixedOr, "(tag:A & level:error) | tag:B"},
		{"tag:A | tag:B level:error", HintMixedOr, "tag:A | (tag:B & level:error)"},
		{"tag:A message:x & message:y | tag:B", HintMixedOr, "(tag:A & message:x & message:y) | tag:B"},
	} {
		h, _ := hintKinds(t, c.q)
		if len(h) != 1 || h[0].Kind != c.kind || h[0].Fix != c.fix {
			t.Errorf("%q: hints %+v, want one %s with fix %q", c.q, h, c.kind, c.fix)
			continue
		}
		// the fix, pasted back, is silent
		if h2, _ := hintKinds(t, h[0].Fix); len(h2) != 0 {
			t.Errorf("%q: fix %q still hints: %+v", c.q, h[0].Fix, h2)
		}
	}
}

func TestHints_silent(t *testing.T) {
	for _, q := range []string{
		"tag:A | tag:B", "tag:A & level:error | tag:B", "(tag:A & level:error) | tag:B",
		"(tag:A | level:error) level:warn", // parenthesized | is not a top-level mix
		"'OR'", `"AND"`, "message:OR", "or", "and", "tag:A or tag:B", "foo AND2",
		"message:'rate limit'", `message:"rate limit"`, "message:rate 'limit'",
		"rate limit", "foo bar baz", // plain implicit terms: deliberately two terms
		"message~:rate limit",          // regex key
		"level:error foo", "pid:1 foo", // not text keys
		"tag:A tag:B", "tag:A", "",
	} {
		if h, _ := hintKinds(t, q); len(h) != 0 {
			t.Errorf("%q: unexpected hints %+v", q, h)
		}
	}
}

func TestHints_capsAndOnePerKind(t *testing.T) {
	// OR twice and a multi-word value twice: one hint per kind
	h, _ := hintKinds(t, "tag:A OR tag:B OR tag:C message:a b message:c d")
	kinds := map[string]int{}
	for _, x := range h {
		kinds[x.Kind]++
	}
	if kinds[HintOrWord] != 1 || kinds[HintMultiWord] > 1 {
		t.Errorf("one per kind: %+v", h)
	}
	// all three kinds: at most 2, in priority order or_word, mixed_or
	h, _ = hintKinds(t, "tag:A OR tag:B level:error | tag:C message:rate limit")
	if len(h) != 2 || h[0].Kind != HintOrWord || h[1].Kind != HintMixedOr {
		t.Errorf("cap: %+v", h)
	}
}

func TestHints_neverChangeTree(t *testing.T) {
	for _, q := range []string{"tag:A OR tag:B", "message:rate limit", "tag:A level:error | tag:B"} {
		with, _ := hintKinds(t, q)
		_ = with
		p1, _ := Parse(q)
		p2, _ := Parse(q)
		p2.Hints = nil
		if !reflect.DeepEqual(p1.Root, p2.Root) || p1.Interpretation() != p2.Interpretation() {
			t.Errorf("%q: tree differs", q)
		}
	}
	// the OR word stays a message keyword (semantics unchanged)
	_, p := hintKinds(t, "tag:A OR tag:B")
	if !strings.Contains(p.Interpretation(), "OR") {
		t.Errorf("OR must stay a keyword: %s", p.Interpretation())
	}
}

func TestHints_windowsSafe(t *testing.T) {
	// fixes use single quotes only: no backslash or double quote to escape in call JSON
	for _, q := range []string{"message:rate limit", "tag:A OR tag:B", "tag:A level:error | tag:B"} {
		h, _ := hintKinds(t, q)
		for _, s := range []string{h[0].Msg, h[0].Fix} {
			if strings.ContainsAny(s, `"\`) {
				t.Errorf("%q: %q has a double quote or backslash", q, s)
			}
		}
	}
}
