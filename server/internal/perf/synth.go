package perf

import (
	"strconv"
	"strings"
	"time"
)

// FixtureDir is the API 29 capture directory relative to a package dir
// under server/internal (server/cmd/<x> uses "../../internal/testdata/api29").
const FixtureDir = "../testdata/api29"

// epochField returns the bounds of the leading `-v epoch` timestamp
// ("1791572202.589379") of a logcat line, or ok=false (dividers,
// continuation lines).
func epochField(ln string) (start, dot, end int, ok bool) {
	i := 0
	for i < len(ln) && ln[i] == ' ' {
		i++
	}
	j := i
	for j < len(ln) && ln[j] >= '0' && ln[j] <= '9' {
		j++
	}
	if j == i || j >= len(ln) || ln[j] != '.' {
		return 0, 0, 0, false
	}
	k := j + 1
	for k < len(ln) && ln[k] >= '0' && ln[k] <= '9' {
		k++
	}
	if k == j+1 || k >= len(ln) || ln[k] != ' ' {
		return 0, 0, 0, false
	}
	return i, j, k, true
}

// Span returns the first and last epoch second of a `-v epoch` capture.
func Span(text string) (first, last int64) {
	for _, ln := range strings.Split(text, "\n") {
		s, d, _, ok := epochField(ln)
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(ln[s:d], 10, 64)
		if err != nil {
			continue
		}
		if first == 0 || sec < first {
			first = sec
		}
		if sec > last {
			last = sec
		}
	}
	return first, last
}

// Retime shifts the epoch timestamp of every line of a `-v epoch` capture
// by whole seconds; everything else is unchanged.
func Retime(text string, shift time.Duration) string {
	sec := int64(shift / time.Second)
	if sec == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + len(text)/64)
	for len(text) > 0 {
		ln := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			ln, text = text[:i+1], text[i+1:]
		} else {
			text = ""
		}
		s, d, _, ok := epochField(ln)
		if !ok {
			b.WriteString(ln)
			continue
		}
		v, err := strconv.ParseInt(ln[s:d], 10, 64)
		if err != nil {
			b.WriteString(ln)
			continue
		}
		b.WriteString(ln[:s])
		b.WriteString(strconv.FormatInt(v+sec, 10))
		b.WriteString(ln[d:])
	}
	return b.String()
}

// Copies calls fn with n back-to-back copies of a capture, copy k shifted
// by k × (its span + gap), so the result reads like one long recording.
// It is the deterministic synthetic history of the benchmarks.
func Copies(text string, n int, gap time.Duration, fn func(k int, text string) error) error {
	first, last := Span(text)
	step := time.Duration(last-first)*time.Second + gap
	for k := 0; k < n; k++ {
		if err := fn(k, Retime(text, time.Duration(k)*step)); err != nil {
			return err
		}
	}
	return nil
}

// CountLines counts non-empty physical lines.
func CountLines(text string) int {
	n := 0
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}
