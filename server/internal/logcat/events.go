package logcat

import "strings"

// EventFields splits an events-buffer message like
// "[0,1234,10123,com.foo,activity,{com.foo/com.foo.Main}]" into its
// top-level fields. At most n fields are returned (the last one keeps any
// remaining commas, useful for free-text "reason" fields); n<=0 = unlimited.
func EventFields(msg string, n int) []string {
	s := strings.TrimSpace(msg)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	s = s[1 : len(s)-1]
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 && (n <= 0 || len(out) < n-1) {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
