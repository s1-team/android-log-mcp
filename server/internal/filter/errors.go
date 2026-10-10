package filter

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Error is a filter parse error with a 1-based column and a fix hint.
type Error struct {
	Input string
	Pos   int // byte offset
	Msg   string
	Hint  string
}

func errAt(input string, pos int, format string, a ...any) *Error {
	return &Error{Input: input, Pos: pos, Msg: fmt.Sprintf(format, a...)}
}

func (e *Error) hint(format string, a ...any) *Error {
	e.Hint = fmt.Sprintf(format, a...)
	return e
}

// Col is the 1-based column (in runes).
func (e *Error) Col() int {
	p := min(max(e.Pos, 0), len(e.Input))
	return utf8.RuneCountInString(e.Input[:p]) + 1
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Filter error at col %d: %s\n", e.Col(), e.Msg)
	in := strings.NewReplacer("\n", " ", "\t", " ", "\r", " ").Replace(e.Input)
	b.WriteString("  " + in + "\n")
	b.WriteString("  " + strings.Repeat(" ", e.Col()-1) + "^")
	if e.Hint != "" {
		b.WriteString("\n" + e.Hint)
	}
	return b.String()
}
