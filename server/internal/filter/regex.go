package filter

import (
	"fmt"
	"regexp"
	"strings"
)

var reChecks = []struct {
	pat  *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\(\?<[=!]`), "look-behind"},
	{regexp.MustCompile(`\(\?[=!]`), "look-ahead"},
	{regexp.MustCompile(`\(\?>`), "atomic groups"},
	{regexp.MustCompile(`(^|[^\\])\\[1-9]`), "backreferences"},
	{regexp.MustCompile(`[*+?}][+]`), "possessive quantifiers"},
	{regexp.MustCompile(`\\[GZ]`), `\G/\Z anchors`},
}

func compileRE2(src string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(src)
	if err == nil {
		return re, nil
	}
	for _, c := range reChecks {
		if c.pat.MatchString(src) {
			return nil, fmt.Errorf("regex %q uses %s, which is not supported (regex engine: Go RE2 — no look-around, no backreferences)", src, c.what)
		}
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "error parsing regexp: ")
	return nil, fmt.Errorf("invalid regex %q: %s (regex engine: Go RE2)", src, msg)
}

func regexHint(keyTok, src string) string {
	switch {
	case strings.Contains(src, "(?<!") || strings.Contains(src, "(?!"):
		return fmt.Sprintf("Rewrite with a positive regex plus a negated term, e.g.: %s\"...\" -%s\"<excluded part>\"", keyTok, strings.TrimPrefix(keyTok, "-"))
	case strings.Contains(src, "(?="), strings.Contains(src, "(?<="):
		return fmt.Sprintf("Drop the look-around and match the text directly, or combine two terms with &, e.g. %s\"a\" & %s\"b\"", keyTok, keyTok)
	}
	return "Use RE2 syntax (https://github.com/google/re2/wiki/Syntax); quote regexes containing spaces or parentheses: " + keyTok + "\"...\""
}
