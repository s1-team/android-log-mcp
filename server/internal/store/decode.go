package store

import (
	"encoding/json"
	"math"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// DecodeRecord decodes one stored record line into r, which must be the
// zero Record. It is equivalent to json.Unmarshal(line, r) (issue #22 D1):
// a hand-written decoder handles the layout the store writes
// ({"s":…,"b":"…","t":…,…}: known keys, compact, integers in range,
// strings with any escapes); any other input, such as an unknown or
// duplicate key, whitespace, a float, null or a syntax error, is decoded
// by encoding/json instead, so results and errors are those of
// encoding/json.
func DecodeRecord(line []byte, r *model.Record) error {
	if decodeFast(line, r) {
		return nil
	}
	*r = model.Record{}
	return json.Unmarshal(line, r)
}

// decodeFast reports false, with r partly filled, for anything outside
// the layout it handles.
func decodeFast(b []byte, r *model.Record) bool {
	n := len(b)
	for n > 0 && isSpace(b[n-1]) { // a trailing newline (bufio.ReadBytes)
		n--
	}
	b = b[:n]
	if len(b) < 2 || b[0] != '{' || b[len(b)-1] != '}' {
		return false
	}
	i := 1
	if b[i] == '}' {
		return i == len(b)-1
	}
	var seen uint16
	for {
		// key: one known letter
		if i+4 > len(b) || b[i] != '"' || b[i+2] != '"' || b[i+3] != ':' {
			return false
		}
		k := b[i+1]
		bit := keyBit(k)
		if bit == 0 || seen&bit != 0 {
			return false // unknown or duplicate key: encoding/json decides
		}
		seen |= bit
		i += 4
		var ok bool
		switch k {
		case 's':
			var v uint64
			v, i, ok = parseUint(b, i)
			r.Seq = v
		case 't':
			r.TimeNs, i, ok = parseInt(b, i)
		case 'u':
			r.UID, i, ok = parseIntField(b, i)
		case 'p':
			r.PID, i, ok = parseIntField(b, i)
		case 'i':
			r.TID, i, ok = parseIntField(b, i)
		case 'l':
			var v uint64
			v, i, ok = parseUint(b, i)
			ok = ok && v <= 255
			r.Level = model.Level(v)
		case 'b':
			r.Boot, i, ok = parseString(b, i)
		case 'g':
			r.Tag, i, ok = parseString(b, i)
		case 'm':
			r.Msg, i, ok = parseString(b, i)
		case 'f':
			r.Buffer, i, ok = parseString(b, i)
		case 'n':
			r.Process, i, ok = parseString(b, i)
		case 'k':
			r.Package, i, ok = parseString(b, i)
		}
		if !ok || i >= len(b) {
			return false
		}
		switch b[i] {
		case ',':
			i++
		case '}':
			return i == len(b)-1
		default:
			return false
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func keyBit(k byte) uint16 {
	switch k {
	case 's':
		return 1 << 0
	case 'b':
		return 1 << 1
	case 't':
		return 1 << 2
	case 'u':
		return 1 << 3
	case 'p':
		return 1 << 4
	case 'i':
		return 1 << 5
	case 'l':
		return 1 << 6
	case 'g':
		return 1 << 7
	case 'm':
		return 1 << 8
	case 'f':
		return 1 << 9
	case 'n':
		return 1 << 10
	case 'k':
		return 1 << 11
	}
	return 0
}

// parseUint reads a JSON integer without sign, fraction or exponent that
// fits in uint64; anything else is not handled.
func parseUint(b []byte, i int) (uint64, int, bool) {
	start := i
	var v uint64
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		d := uint64(b[i] - '0')
		if v > (math.MaxUint64-d)/10 {
			return 0, i, false // overflow: encoding/json reports it
		}
		v = v*10 + d
		i++
	}
	if i == start || (i-start > 1 && b[start] == '0') {
		return 0, i, false
	}
	if i < len(b) && (b[i] == '.' || b[i] == 'e' || b[i] == 'E') {
		return 0, i, false
	}
	return v, i, true
}

// parseIntField is parseInt for an int field (range of int on this
// platform, as encoding/json checks).
func parseIntField(b []byte, i int) (int, int, bool) {
	v, i, ok := parseInt(b, i)
	if v < math.MinInt || v > math.MaxInt {
		return 0, i, false
	}
	return int(v), i, ok
}

// parseInt is parseUint with an optional minus sign, in int64 range.
func parseInt(b []byte, i int) (int64, int, bool) {
	neg := i < len(b) && b[i] == '-'
	if neg {
		i++
	}
	u, i, ok := parseUint(b, i)
	switch {
	case !ok:
		return 0, i, false
	case neg && u <= 1<<63:
		return -int64(u), i, true // -(1<<63) wraps to MinInt64
	case !neg && u <= math.MaxInt64:
		return int64(u), i, true
	}
	return 0, i, false
}

// parseString reads a JSON string at b[i] (the opening quote) with
// encoding/json's semantics: escapes, \u with surrogate pairs (a lone
// surrogate becomes U+FFFD), invalid UTF-8 bytes each become U+FFFD; a raw
// control character is a syntax error (not handled here).
func parseString(b []byte, i int) (string, int, bool) {
	if i >= len(b) || b[i] != '"' {
		return "", i, false
	}
	i++
	start := i
	// fast scan: plain ASCII without escapes
	for i < len(b) {
		c := b[i]
		if c == '"' {
			return string(b[start:i]), i + 1, true
		}
		if c == '\\' || c < 0x20 || c >= utf8.RuneSelf {
			break
		}
		i++
	}
	out := make([]byte, 0, i-start+16)
	out = append(out, b[start:i]...)
	for i < len(b) {
		c := b[i]
		switch {
		case c == '"':
			return string(out), i + 1, true
		case c < 0x20:
			return "", i, false
		case c == '\\':
			if i+1 >= len(b) {
				return "", i, false
			}
			switch b[i+1] {
			case '"', '\\', '/':
				out = append(out, b[i+1])
				i += 2
			case 'b':
				out = append(out, '\b')
				i += 2
			case 'f':
				out = append(out, '\f')
				i += 2
			case 'n':
				out = append(out, '\n')
				i += 2
			case 'r':
				out = append(out, '\r')
				i += 2
			case 't':
				out = append(out, '\t')
				i += 2
			case 'u':
				rr := hex4(b, i+2)
				if rr < 0 {
					return "", i, false
				}
				i += 6
				if utf16.IsSurrogate(rune(rr)) {
					rr2 := -1
					if i+6 <= len(b) && b[i] == '\\' && b[i+1] == 'u' {
						rr2 = hex4(b, i+2)
					}
					if dec := utf16.DecodeRune(rune(rr), rune(rr2)); rr2 >= 0 && dec != utf8.RuneError {
						i += 6
						out = utf8.AppendRune(out, dec)
						break
					}
					rr = utf8.RuneError
				}
				out = utf8.AppendRune(out, rune(rr))
			default:
				return "", i, false
			}
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			rr, size := utf8.DecodeRune(b[i:])
			if rr == utf8.RuneError && size == 1 {
				out = utf8.AppendRune(out, utf8.RuneError)
			} else {
				out = append(out, b[i:i+size]...)
			}
			i += size
		}
	}
	return "", i, false
}

// hex4 parses four hex digits at b[i:], or returns -1.
func hex4(b []byte, i int) int {
	if i+4 > len(b) {
		return -1
	}
	v := 0
	for _, c := range b[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		v = v<<4 | int(c)
	}
	return v
}
