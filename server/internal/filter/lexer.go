// Package filter implements the Android Studio Logcat filter language
// (lexer/parser/evaluator) with strict errors instead of AS's silent
// "match the whole query as text" fallback, plus a few documented extensions.
//
// Reference: AOSP platform/tools/adt/idea studio-main
// logcat/src/com/android/tools/idea/logcat/filters/{parser/LogcatFilter.flex,
// parser/LogcatFilter.bnf, LogcatFilterParser.kt, LogcatFilter.kt}.
package filter

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tkValue    tokKind = iota // free text (AS VALUE)
	tkStrKey                  // [-]key: [-]key=:   (AS STRING_KEY)
	tkRegexKey                // [-]key~:           (AS REGEX_KEY)
	tkKey                     // age: level: is: name: (AS KEY) + ext pid: tid: buffer:
	tkKValue                  // value following a key
	tkAnd
	tkOr
	tkLParen
	tkRParen
	tkEOF
)

type token struct {
	kind  tokKind
	text  string // raw source text
	pos   int    // byte offset
	end   int    // byte offset (exclusive)
	wsAft bool   // followed by whitespace (or EOF)
	// for key tokens
	key    string
	neg    bool
	op     string // ":", "=:", "~:"
	quoted bool   // for values
}

// AS text keys (accept quoted values, negation, =: and ~:).
var textKeys = []string{"line", "message", "package", "process", "tag"}

// AS regular keys (value is \S+; no negation/exact/regex variants).
var regularKeys = []string{"age", "level", "is", "name"}

// Extension keys (not in Android Studio). Negatable with '-'.
var extKeys = []string{"pid", "tid", "buffer"}

// AllKeys is the list shown in error messages.
var AllKeys = []string{"tag", "message", "package", "process", "line", "level", "age", "is", "pid", "tid", "buffer"}

func isSpace(r rune) bool { return unicode.IsSpace(r) }

// matchKeyAt returns the length of a key token at s[i:] (longest match), or 0.
func matchKeyAt(s string, i int) (n int, kind tokKind, key string, neg bool, op string) {
	j := i
	if j < len(s) && s[j] == '-' {
		neg = true
		j++
	}
	for _, k := range textKeys {
		if strings.HasPrefix(s[j:], k) {
			r := s[j+len(k):]
			switch {
			case strings.HasPrefix(r, ":"):
				return j + len(k) + 1 - i, tkStrKey, k, neg, ":"
			case strings.HasPrefix(r, "=:"):
				return j + len(k) + 2 - i, tkStrKey, k, neg, "=:"
			case strings.HasPrefix(r, "~:"):
				return j + len(k) + 2 - i, tkRegexKey, k, neg, "~:"
			}
		}
	}
	for _, k := range extKeys {
		if strings.HasPrefix(s[j:], k+":") {
			return j + len(k) + 1 - i, tkKey, k, neg, ":"
		}
	}
	if !neg {
		for _, k := range regularKeys {
			if strings.HasPrefix(s[i:], k+":") {
				return len(k) + 1, tkKey, k, false, ":"
			}
		}
	}
	return 0, 0, "", false, ""
}

// unquotedLen implements UNQUOTED_VALUE = [^'"\s()] ([^\s():] | "\\ ")*
// Extension: "\:", "\(", "\)" are also accepted inside unquoted values.
func unquotedLen(s string, i int) int {
	r, sz := utf8.DecodeRuneInString(s[i:])
	if r == '\'' || r == '"' || r == '(' || r == ')' || isSpace(r) {
		return 0
	}
	j := i + sz
	for j < len(s) {
		r, sz = utf8.DecodeRuneInString(s[j:])
		if r == '\\' && j+1 < len(s) {
			n := s[j+1]
			if n == ' ' || n == ':' || n == '(' || n == ')' {
				j += 2
				continue
			}
		}
		if isSpace(r) || r == '(' || r == ')' || r == ':' {
			break
		}
		j += sz
	}
	return j - i
}

// quotedLen implements ' ([^'] | \\')* ' (and the " variant) with JFlex
// longest-match semantics. Returns 0 if unterminated.
func quotedLen(s string, i int) int {
	q := s[i]
	best := 0
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if s[j-1] == '\\' && j-1 > i {
			best = j + 1 - i // may close here, but an escaped quote may also continue
			continue
		}
		return j + 1 - i
	}
	return best
}

func valueLen(s string, i int) (int, bool) {
	if s[i] == '\'' || s[i] == '"' {
		return quotedLen(s, i), true
	}
	return unquotedLen(s, i), false
}

// lex tokenizes the query. It returns a *Error for unlexable input.
func lex(s string) ([]token, *Error) {
	var toks []token
	i := 0
	expectKV := tokKind(-1) // after a key: which value kind to read
	var lastKey *token
	for {
		for i < len(s) {
			r, sz := utf8.DecodeRuneInString(s[i:])
			if !isSpace(r) {
				break
			}
			if len(toks) > 0 {
				toks[len(toks)-1].wsAft = true
			}
			i += sz
		}
		if i >= len(s) {
			if expectKV >= 0 {
				return nil, errAt(s, lastKey.pos, "missing value after %q", lastKey.text).
					hint("Write e.g. %sfoo, or quote values with spaces: %s\"foo bar\"", lastKey.text, lastKey.text)
			}
			if len(toks) > 0 {
				toks[len(toks)-1].wsAft = true
			}
			toks = append(toks, token{kind: tkEOF, pos: len(s), end: len(s)})
			return toks, nil
		}
		if expectKV >= 0 {
			var n int
			quoted := false
			if lastKey.kind == tkKey {
				// AS: KVALUE = \S+ . Extension: stop at ')' so "(level:error)" works.
				j := i
				for j < len(s) {
					r, sz := utf8.DecodeRuneInString(s[j:])
					if isSpace(r) || r == ')' {
						break
					}
					j += sz
				}
				n = j - i
			} else {
				n, quoted = valueLen(s, i)
				if quoted && n == 0 {
					return nil, errAt(s, i, "unterminated %c-quoted value", s[i]).
						hint("Close the quote: %s%s%c", lastKey.text, s[i:], s[i])
				}
			}
			if n == 0 {
				return nil, errAt(s, i, "missing value after %q (found %q)", lastKey.text, string(s[i])).
					hint("Quote values that start with '(' or ')': %s\"%s\"", lastKey.text, firstWord(s[i:]))
			}
			toks = append(toks, token{kind: tkKValue, text: s[i : i+n], pos: i, end: i + n, quoted: quoted})
			i += n
			expectKV = -1
			continue
		}
		// YYINITIAL: longest match; ties resolved by rule order (keys, operators, value).
		kn, kk, key, neg, op := matchKeyAt(s, i)
		vn, quoted := valueLen(s, i)
		if quoted && vn == 0 {
			return nil, errAt(s, i, "unterminated %c-quoted value", s[i]).
				hint("Close the quote: %s%c", s[i:], s[i])
		}
		opn := 0
		var opk tokKind
		switch s[i] {
		case '|':
			opn, opk = 1, tkOr
		case '&':
			opn, opk = 1, tkAnd
		case '(':
			opn, opk = 1, tkLParen
		case ')':
			opn, opk = 1, tkRParen
		}
		switch {
		case kn > 0 && kn >= vn:
			toks = append(toks, token{kind: kk, text: s[i : i+kn], pos: i, end: i + kn, key: key, neg: neg, op: op})
			lastKey = &toks[len(toks)-1]
			expectKV = kk
			i += kn
		case opn > 0 && opn >= vn:
			toks = append(toks, token{kind: opk, text: s[i : i+1], pos: i, end: i + 1})
			i++
		case vn > 0:
			toks = append(toks, token{kind: tkValue, text: s[i : i+vn], pos: i, end: i + vn, quoted: quoted})
			i += vn
		default:
			return nil, errAt(s, i, "unexpected character %q", string(s[i]))
		}
	}
}

func firstWord(s string) string {
	if i := strings.IndexFunc(s, isSpace); i >= 0 {
		return s[:i]
	}
	return s
}

var escapedChar = strings.NewReplacer(`\ `, " ", `\:`, ":", `\'`, "'", `\"`, `"`, `\\`, `\`, `\(`, "(", `\)`, ")")

// unescape mirrors PsiExtensions.toText.
func unescape(t string) string {
	if len(t) >= 2 && t[0] == '\'' && t[len(t)-1] == '\'' {
		return strings.ReplaceAll(t[1:len(t)-1], `\'`, "'")
	}
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		return strings.ReplaceAll(t[1:len(t)-1], `\"`, `"`)
	}
	return escapedChar.Replace(t)
}
