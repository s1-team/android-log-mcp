package mcpserver

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Short stacks in logcat_search (stack="short", #23). A Java stack logged
// with a line (Log.w(TAG, msg, e)) is one record of 20–40 frame lines, about
// 1 KB; shown in full it dominates the size of a response whatever the limit.

const shortFrames = 3 // frames kept per exception and per Caused by

// stackStats counts what stack="short" left out of one response.
type stackStats struct {
	shortened int // lines whose stack was shortened or referenced
	hidden    int // frames not shown
}

type sectionKind int

const (
	secMain     sectionKind = iota // the message and the thrown exception (or any other block)
	secCause                       // "Caused by: …"
	secSuppress                    // "Suppressed: …"
)

// stackSection is one exception of a stack: its head lines, its frames and
// the frames Java itself elided ("... N more").
type stackSection struct {
	kind   sectionKind
	head   []string
	frames []string
	more   int
	indent string // of the frames, for the ↳ line
}

func (s *stackSection) total() int { return len(s.frames) + s.more }

func isFrame(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "at ") }

// moreFrames parses Java's "... 22 more".
func moreFrames(l string) (int, bool) {
	t := strings.TrimSpace(l)
	if !strings.HasPrefix(t, "... ") || !strings.HasSuffix(t, " more") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(t, "... "), " more"))
	return n, err == nil
}

func leadingSpace(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }

func parseStack(msg string) []*stackSection {
	cur := &stackSection{kind: secMain}
	out := []*stackSection{cur}
	for _, l := range strings.Split(msg, "\n") {
		if isFrame(l) {
			if len(cur.frames) == 0 && cur.more == 0 {
				cur.indent = leadingSpace(l)
			}
			cur.frames = append(cur.frames, l)
			continue
		}
		if n, ok := moreFrames(l); ok {
			if cur.indent == "" {
				cur.indent = leadingSpace(l)
			}
			cur.more += n
			continue
		}
		t := strings.TrimSpace(l)
		kind := secMain
		switch {
		case strings.HasPrefix(t, "Caused by:"):
			kind = secCause
		case strings.HasPrefix(t, "Suppressed:"):
			kind = secSuppress
		}
		if kind != secMain || len(cur.frames) > 0 || cur.more > 0 {
			cur = &stackSection{kind: kind}
			out = append(out, cur)
		}
		cur.head = append(cur.head, l)
	}
	return out
}

// renderSections writes the sections keeping n frames of each (Suppressed:
// keeps none) and returns the text and the frames left out.
func renderSections(secs []*stackSection, nMain, nCause int) (string, int) {
	var lines []string
	hidden := 0
	for _, s := range secs {
		n := nMain
		switch s.kind {
		case secCause:
			n = nCause
		case secSuppress:
			n = 0
		}
		lines = append(lines, s.head...)
		n = min(n, len(s.frames))
		lines = append(lines, s.frames[:n]...)
		if h := s.total() - n; h > 0 {
			ind := s.indent
			if ind == "" {
				ind = "\t"
			}
			lines = append(lines, fmt.Sprintf("%s%s frames_hidden=%d", ind, identicalLabel, h))
			hidden += h
		}
	}
	return strings.Join(lines, "\n"), hidden
}

// shortStack shortens a stack: every head line (the message, the
// exception, each Caused by and Suppressed), the first 3 frames of each
// exception and Caused by, and "↳ K more frames" for the rest (Java's
// "... N more" and a Suppressed exception's frames included). Over
// shortStackChars it keeps fewer frames; every Caused by head and its first
// frame are kept before anything else (the last Caused by is usually the
// root cause). changed reports whether anything was left out.
func shortStack(msg string) (out string, hidden int, changed bool) {
	secs := parseStack(msg)
	for _, plan := range [][2]int{{shortFrames, shortFrames}, {1, 1}, {0, 1}} {
		out, hidden = renderSections(secs, plan[0], plan[1])
		if len(out) <= shortStackChars {
			return out, hidden, out != msg
		}
	}
	// still too long (a long message or many causes): keep the causes
	// (head + first frame, each line at most 300 chars) and cut the rest
	var front, causes []*stackSection
	for i, s := range secs {
		if s.kind == secCause {
			front, causes = secs[:i], secs[i:]
			break
		}
	}
	if causes == nil {
		front = secs
	}
	frontText, h1 := renderSections(front, 0, 1)
	var tail string
	h2 := 0
	if len(causes) > 0 {
		// shorten the causes' long lines until they leave room for the head
		for _, n := range []int{300, 200, 120, 80} {
			var cs []*stackSection
			for _, s := range causes {
				c := *s
				c.head = clipLines(s.head, n)
				c.frames = clipLines(s.frames, n)
				cs = append(cs, &c)
			}
			tail, h2 = renderSections(cs, 0, 1)
			tail = "\n" + tail
			if len(tail) <= shortStackChars-200 {
				break
			}
		}
	}
	if room := max(shortStackChars-len(tail), 200); len(frontText) > room {
		marker := lineCut(shortStackChars, len(frontText), `stack="full"`)
		k := cutAt(frontText, max(room-len(marker), 100))
		frontText = frontText[:k] + lineCut(shortStackChars, len(frontText)-k, `stack="full"`)
	}
	return frontText + tail, h1 + h2, true
}

// clipLines cuts each line to at most n bytes (plus "…").
func clipLines(ls []string, n int) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		if len(l) > n {
			l = l[:cutAt(l, n)] + "..."
		}
		out[i] = l
	}
	return out
}

// stackKey identifies a stack by its frames and causes: the message from
// its first frame line on ("" if there is none).
func stackKey(msg string) string {
	for i := 0; i < len(msg); {
		j := strings.IndexByte(msg[i:], '\n')
		line := msg[i:]
		if j >= 0 {
			line = msg[i : i+j]
		}
		if isFrame(line) {
			return msg[i:]
		}
		if j < 0 {
			break
		}
		i += j + 1
	}
	return ""
}

// sameStack renders a stack whose frames were printed earlier in the
// response (at): every head line, then "↳ same K frames as <time>". Over
// shortStackChars it is shortened like any stack instead.
func sameStack(msg string, at int64, loc *time.Location) (string, int) {
	secs := parseStack(msg)
	var lines []string
	total := 0
	ind := ""
	for _, s := range secs {
		lines = append(lines, s.head...)
		total += s.total()
		if ind == "" {
			ind = s.indent
		}
	}
	if ind == "" {
		ind = "\t"
	}
	lines = append(lines, fmt.Sprintf("%s%s same_frames=%d as=%s", ind, identicalLabel, total, clock(at, loc)))
	out := strings.Join(lines, "\n")
	if len(out) > shortStackChars { // long heads: shortStack keeps the causes first
		out, total, _ = shortStack(msg)
	}
	return out, total
}
