package logcat

import (
	"strings"
	"unicode/utf8"
)

// UnescapePrintable undoes `logcat -v printable` escaping of non-ASCII bytes:
// runs of octal (`\303\251`, Android ≤ 12) or hex (`\xC3\xA9`, newer) escapes
// whose bytes are ≥ 0x80 are decoded back to UTF-8. Bytes that do not form
// valid UTF-8 keep their escaped spelling, so the result is always valid
// UTF-8 (safe for JSON) and nothing is lost. C escapes for control
// characters (\n, \b, …) and an escaped backslash (`\\`) are left as they are.
func UnescapePrintable(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '\\' {
			b.WriteString(`\\`)
			i += 2
			continue
		}
		// collect a run of high-byte escapes
		var raw []byte
		var spell []string
		j := i
		for j < len(s) {
			v, n := highEscape(s[j:])
			if n == 0 {
				break
			}
			raw = append(raw, v)
			spell = append(spell, s[j:j+n])
			j += n
		}
		if len(raw) == 0 {
			b.WriteByte('\\')
			i++
			continue
		}
		for k := 0; k < len(raw); {
			r, size := utf8.DecodeRune(raw[k:])
			if r == utf8.RuneError && size <= 1 {
				b.WriteString(spell[k])
				k++
				continue
			}
			b.WriteRune(r)
			k += size
		}
		i = j
	}
	return b.String()
}

// highEscape decodes one `\ooo` or `\xHH` escape of a byte ≥ 0x80 at the
// start of s; n == 0 when there is none.
func highEscape(s string) (v byte, n int) {
	if len(s) >= 4 && s[0] == '\\' && isOct(s[1]) && isOct(s[2]) && isOct(s[3]) {
		x := int(s[1]-'0')<<6 | int(s[2]-'0')<<3 | int(s[3]-'0')
		if x >= 0x80 && x <= 0xff {
			return byte(x), 4
		}
		return 0, 0
	}
	if len(s) >= 4 && s[0] == '\\' && s[1] == 'x' && isHex(s[2]) && isHex(s[3]) {
		x := hexVal(s[2])<<4 | hexVal(s[3])
		if x >= 0x80 {
			return byte(x), 4
		}
	}
	return 0, 0
}

func isOct(c byte) bool { return c >= '0' && c <= '7' }
func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}
