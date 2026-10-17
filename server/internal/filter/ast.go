package filter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Field is a text field addressed by a key.
type Field int

const (
	FTag Field = iota
	FPackage
	FMessage
	FLine
	FImplicitLine // bare words
	FProcess
)

func (f Field) String() string {
	return [...]string{"tag", "package", "message", "line", "line", "process"}[f]
}

// Ctx carries evaluation-time context.
type Ctx struct {
	Now time.Time      // device "now" for age:
	Loc *time.Location // device zone for the canonical line
}

// Node is a compiled filter predicate.
type Node interface {
	Match(r *model.Record, c *Ctx) bool
	// String renders the normalized interpretation.
	String() string
}

func fieldValue(f Field, r *model.Record, c *Ctx) string {
	switch f {
	case FTag:
		return r.Tag
	case FPackage:
		return r.AppID()
	case FMessage:
		return r.Msg
	case FProcess:
		return r.Process
	default:
		return r.Line(c.Loc)
	}
}

type And struct{ Kids []Node }
type Or struct{ Kids []Node }
type Empty struct{}

func (n *And) Match(r *model.Record, c *Ctx) bool {
	for _, k := range n.Kids {
		if !k.Match(r, c) {
			return false
		}
	}
	return true
}
func (n *Or) Match(r *model.Record, c *Ctx) bool {
	for _, k := range n.Kids {
		if k.Match(r, c) {
			return true
		}
	}
	return false
}
func (Empty) Match(*model.Record, *Ctx) bool { return true }
func (Empty) String() string                 { return "(empty)" }

func (n *And) String() string { return joinKids(n.Kids, " AND ") }
func (n *Or) String() string  { return joinKids(n.Kids, " OR ") }

func joinKids(kids []Node, sep string) string {
	parts := make([]string, len(kids))
	for i, k := range kids {
		s := k.String()
		switch k.(type) {
		case *And, *Or:
			s = "(" + s + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, sep)
}

// Str is contains (case-insensitive) or exact (case-sensitive) matching.
type Str struct {
	Field Field
	Value string
	Exact bool
	Neg   bool
	lower string
}

func (n *Str) Match(r *model.Record, c *Ctx) bool {
	v := fieldValue(n.Field, r, c)
	var m bool
	if n.Exact {
		m = v == n.Value
	} else {
		m = containsFold(v, n.lower)
	}
	return m != n.Neg
}

func (n *Str) String() string {
	op := "contains"
	if n.Exact {
		op = "=="
	}
	s := fmt.Sprintf("%s %s %s", n.Field, op, strconv.Quote(n.Value))
	if n.Neg {
		s = "NOT " + s
	}
	return s
}

// containsFold reports whether lower(v) contains sub (already lowered).
func containsFold(v, lowerSub string) bool {
	if lowerSub == "" {
		return true
	}
	// Fast path for ASCII.
	if isASCII(v) {
		n := len(lowerSub)
		for i := 0; i+n <= len(v); i++ {
			ok := true
			for j := 0; j < n; j++ {
				ch := v[i+j]
				if 'A' <= ch && ch <= 'Z' {
					ch += 'a' - 'A'
				}
				if ch != lowerSub[j] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	return strings.Contains(strings.ToLower(v), lowerSub)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Regex is RE2 partial matching (case-sensitive unless (?i)).
type Regex struct {
	Field Field
	Src   string
	Re    *regexp.Regexp
	Neg   bool
}

func (n *Regex) Match(r *model.Record, c *Ctx) bool {
	return n.Re.MatchString(fieldValue(n.Field, r, c)) != n.Neg
}
func (n *Regex) String() string {
	s := fmt.Sprintf("%s ~ /%s/", n.Field, n.Src)
	if n.Neg {
		s = "NOT " + s
	}
	return s
}

// Level matches priority >= Min.
type Level struct{ Min model.Level }

func (n *Level) Match(r *model.Record, _ *Ctx) bool { return r.Level.Rank() >= n.Min.Rank() }
func (n *Level) String() string                     { return "level >= " + n.Min.Name() }

// Age matches entries no older than Dur relative to Ctx.Now.
type Age struct {
	Dur time.Duration
	Src string
}

func (n *Age) Match(r *model.Record, c *Ctx) bool {
	return c.Now.UnixNano()-r.TimeNs <= n.Dur.Nanoseconds()
}
func (n *Age) String() string { return "age <= " + n.Src }

// Crash is AS's is:crash.
type Crash struct{}

func (Crash) Match(r *model.Record, _ *Ctx) bool { return IsCrashRecord(r) }
func (Crash) String() string                     { return "is crash" }

// IsCrashRecord: (E && tag==AndroidRuntime && msg starts "FATAL EXCEPTION") || (ASSERT && tag in {DEBUG, libc}).
func IsCrashRecord(r *model.Record) bool {
	return (r.Level == model.Error && r.Tag == "AndroidRuntime" && strings.HasPrefix(r.Msg, "FATAL EXCEPTION")) ||
		(r.Level.Rank() == 6 && (r.Tag == "DEBUG" || r.Tag == "libc"))
}

var exceptionLine = regexp.MustCompile(`\n\s*at .+\(.+\)\n`)

// StackTrace is AS's is:stacktrace.
type StackTrace struct{}

func (StackTrace) Match(r *model.Record, _ *Ctx) bool { return exceptionLine.MatchString(r.Msg) }
func (StackTrace) String() string                     { return "is stacktrace" }

// Num is the pid:/tid: extension.
type Num struct {
	Key string
	N   int
	Neg bool
}

func (n *Num) Match(r *model.Record, _ *Ctx) bool {
	v := r.PID
	if n.Key == "tid" {
		v = r.TID
	}
	return (v == n.N) != n.Neg
}
func (n *Num) String() string {
	s := fmt.Sprintf("%s == %d [ext]", n.Key, n.N)
	if n.Neg {
		s = "NOT " + s
	}
	return s
}

// Buffer is the buffer: extension.
type Buffer struct {
	Name string
	Neg  bool
}

func (n *Buffer) Match(r *model.Record, _ *Ctx) bool { return (r.Buffer == n.Name) != n.Neg }
func (n *Buffer) String() string {
	s := "buffer == " + n.Name + " [ext]"
	if n.Neg {
		s = "NOT " + s
	}
	return s
}

// Pred wraps an arbitrary predicate (used for structured params).
type Pred struct {
	Desc string
	Fn   func(r *model.Record, c *Ctx) bool
	// ClassOnly: Match depends only on the class key (package / process / uid /
	// tag / level; uid is not a filter term, `uid:` aliases `package`)
	// (issue #37). Such a Pred is countable via the per-block class index.
	ClassOnly bool
}

func (n *Pred) Match(r *model.Record, c *Ctx) bool { return n.Fn(r, c) }
func (n *Pred) String() string                     { return n.Desc }
