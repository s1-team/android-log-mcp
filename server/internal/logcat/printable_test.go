package logcat

import (
	"testing"
	"unicode/utf8"
)

func TestUnescapePrintable(t *testing.T) {
	for in, want := range map[string]string{
		`Updating clock: 3\356\270\20126`: "Updating clock: 3\uee0126", // private-use glyph from the API 29 fixture
		`caf\303\251`:                     "café",
		`\316\221\316\222`:                "ΑΒ",
		`caf\xC3\xA9`:                     "café",
		`bad \377 byte`:                   `bad \377 byte`, // invalid UTF-8 stays escaped
		`half \303`:                       `half \303`,
		`ctrl \b and \n`:                  `ctrl \b and \n`,
		`back\\303\251`:                   `back\\303\251`, // escaped backslash, then a stray continuation byte
		`ascii \101`:                      `ascii \101`,    // < 0x80 is not something printable escapes
		`no escapes`:                      `no escapes`,
	} {
		got := UnescapePrintable(in)
		if got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%q: invalid utf8", in)
		}
	}
}

func TestParseLineUnescapes(t *testing.T) {
	// verbatim from internal/testdata/api29/session3-raw.log
	l := ParseLine(`         1791574020.036473 10114  2225  2225 D KeyguardClockSwitch: Updating clock: 3\356\270\20127`, nil, 2026)
	if !l.OK || l.Msg != "Updating clock: 3\uee0127" {
		t.Fatalf("%+v", l)
	}
}
