package filter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Options mirror LogcatFilterParser constructor knobs. Production Android
// Studio uses JoinConsecutiveValues=false, SameKeyAND=false (same-key OR).
type Options struct {
	JoinConsecutiveValues bool
	SameKeyAND            bool
}

// Parsed is the result of a successful parse.
type Parsed struct {
	Root  Node     // nil for an empty filter
	Notes []string // warnings (never errors)
}

// Interpretation renders the normalized predicate.
func (p *Parsed) Interpretation() string {
	if p == nil || p.Root == nil {
		return "(none)"
	}
	return p.Root.String()
}

// Parse parses with production Android Studio semantics.
func Parse(s string) (*Parsed, error) { return ParseWith(s, Options{}) }

type parser struct {
	src   string
	toks  []token
	i     int
	opts  Options
	notes []string
}

// ParseWith parses s. Errors are *Error.
func ParseWith(s string, opts Options) (*Parsed, error) {
	if strings.TrimSpace(s) == "" {
		// AS: "" -> no filter; blank -> line contains blank. We treat both as no filter.
		return &Parsed{}, nil
	}
	toks, lerr := lex(s)
	if lerr != nil {
		return nil, lerr
	}
	p := &parser{src: s, toks: toks, opts: opts}
	if err := p.checkAdjacency(); err != nil {
		return nil, err
	}
	var tops []item
	for p.peek().kind != tkEOF {
		if p.peek().kind == tkRParen {
			return nil, errAt(s, p.peek().pos, "unmatched ')'").
				hint("Remove it or add the matching '(': %s", suggestBalanced(s))
		}
		start := p.peek()
		n, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		tops = append(tops, item{node: n, isValue: start.kind == tkValue && p.toks[p.i-1].pos == start.pos, tok: start})
	}
	root := p.combineTop(tops)
	return &Parsed{Root: root, Notes: p.notes}, nil
}

type item struct {
	node    Node
	isValue bool // a bare top-level VALUE literal
	tok     token
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) orExpr() (Node, error) {
	first, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	kids := []Node{first}
	for p.peek().kind == tkOr {
		op := p.next()
		if !startsPrimary(p.peek().kind) {
			return nil, errAt(p.src, op.pos, "'|' needs an expression on its right").
				hint("Example: tag:A | tag:B")
		}
		n, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return &Or{Kids: kids}, nil
}

func (p *parser) andExpr() (Node, error) {
	first, err := p.primary()
	if err != nil {
		return nil, err
	}
	kids := []Node{first}
	for p.peek().kind == tkAnd {
		op := p.next()
		if !startsPrimary(p.peek().kind) {
			return nil, errAt(p.src, op.pos, "'&' needs an expression on its right").
				hint("Example: message:foo & message:bar")
		}
		n, err := p.primary()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return &And{Kids: kids}, nil
}

// The operator chain is built flat (AS flattenAnd/OrExpression); parenthesized
// sub-expressions stay nested.

func startsPrimary(k tokKind) bool {
	switch k {
	case tkValue, tkStrKey, tkRegexKey, tkKey, tkLParen:
		return true
	}
	return false
}

func (p *parser) primary() (Node, error) {
	t := p.peek()
	switch t.kind {
	case tkLParen:
		p.next()
		if p.peek().kind == tkRParen {
			p.next()
			return Empty{}, nil
		}
		if p.peek().kind == tkEOF {
			return nil, errAt(p.src, t.pos, "unclosed '('").hint("Add the closing ')': %s", suggestBalanced(p.src))
		}
		n, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		switch p.peek().kind {
		case tkRParen:
			p.next()
			return n, nil
		case tkEOF:
			return nil, errAt(p.src, t.pos, "unclosed '('").hint("Add the closing ')': %s", suggestBalanced(p.src))
		default:
			return nil, errAt(p.src, p.peek().pos, "inside parentheses terms must be joined with & or |").
				hint("e.g. (tag:a & level:error) or (tag:a | tag:b)")
		}
	case tkValue:
		p.next()
		if t.text == "&&" || t.text == "||" {
			return nil, errAt(p.src, t.pos, "%q is not an operator", t.text).
				hint("Use a single %c with spaces around it, e.g. tag:A %c tag:B", t.text[0], t.text[0])
		}
		v := unescape(t.text)
		p.noteOperatorInValue(v, "", t)
		return &Str{Field: FImplicitLine, Value: v, lower: strings.ToLower(v)}, nil
	case tkStrKey, tkRegexKey, tkKey:
		p.next()
		vt := p.next() // lexer guarantees a value
		return p.keyLiteral(t, vt)
	case tkAnd, tkOr:
		return nil, errAt(p.src, t.pos, "'%s' needs an expression on its left", t.text).
			hint("Example: tag:A %s tag:B", t.text)
	case tkRParen:
		return nil, errAt(p.src, t.pos, "unexpected ')'").hint("Balanced form: %s", suggestBalanced(p.src))
	default:
		return nil, errAt(p.src, t.pos, "unexpected end of filter")
	}
}

func fieldOf(key string) Field {
	switch key {
	case "tag":
		return FTag
	case "package":
		return FPackage
	case "message":
		return FMessage
	case "process":
		return FProcess
	}
	return FLine
}

func (p *parser) keyLiteral(kt, vt token) (Node, error) {
	raw := vt.text
	switch kt.key {
	case "level":
		lv, ok := ParseLevel(raw)
		if !ok {
			return nil, errAt(p.src, vt.pos, "level:%q is not a level", raw).
				hint("Use one of verbose debug info warn error assert (or V D I W E A F), e.g. level:warn")
		}
		return &Level{Min: lv}, nil
	case "age":
		d, ok := parseAge(raw)
		if !ok {
			return nil, errAt(p.src, vt.pos, "age:%q — use <number><s|m|h|d>", raw).hint("e.g. age:10m")
		}
		return &Age{Dur: d, Src: raw}, nil
	case "is":
		switch raw {
		case "crash":
			return Crash{}, nil
		case "stacktrace":
			return StackTrace{}, nil
		case "firebase":
			return &Regex{Field: FTag, Src: firebaseRegex, Re: regexp.MustCompile(firebaseRegex)}, nil
		}
		return nil, errAt(p.src, vt.pos, "is:%q is not supported", raw).hint("Use is:crash, is:stacktrace or is:firebase (lowercase)")
	case "name":
		return nil, errAt(p.src, kt.pos, "name: is not supported here (it only names a saved filter in Android Studio)").
			hint("Remove the name:%s term", raw)
	case "pid", "tid":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, errAt(p.src, vt.pos, "%s:%q is not a number", kt.key, raw).hint("e.g. %s:12345", kt.key)
		}
		return &Num{Key: kt.key, N: n, Neg: kt.neg}, nil
	case "buffer":
		switch raw {
		case "main", "system", "crash", "events":
			return &Buffer{Name: raw, Neg: kt.neg}, nil
		}
		return nil, errAt(p.src, vt.pos, "buffer:%q is not a recorded buffer", raw).hint("Use one of main system crash events")
	}
	// text keys
	v := unescape(raw)
	f := fieldOf(kt.key)
	if kt.op == "~:" {
		re, err := compileRE2(v)
		if err != nil {
			return nil, errAt(p.src, vt.pos, "%s", err.Error()).hint("%s", regexHint(kt.text, v))
		}
		return &Regex{Field: f, Src: v, Re: re, Neg: kt.neg}, nil
	}
	if kt.key == "package" && v == "mine" && !kt.neg && kt.op == ":" {
		return nil, errAt(p.src, kt.pos, "package:mine is not supported here (no IDE project)").
			hint(`Use the package parameter instead: package="com.foo"`)
	}
	if !vt.quoted {
		p.noteOperatorInValue(v, kt.text, vt)
	}
	return &Str{Field: f, Value: v, Exact: kt.op == "=:", Neg: kt.neg, lower: strings.ToLower(v)}, nil
}

func (p *parser) noteOperatorInValue(v, key string, t token) {
	if t.quoted {
		return
	}
	for _, op := range []string{"|", "&"} {
		if strings.Contains(v, op) && v != op {
			parts := strings.Split(v, op)
			alt := make([]string, 0, len(parts))
			for _, x := range parts {
				alt = append(alt, key+x)
			}
			p.notes = append(p.notes, fmt.Sprintf("value %q contains %q — if you meant an operator write %s", v, op, strings.Join(alt, " "+op+" ")))
			return
		}
	}
}

// ParseLevel accepts AS names (case-insensitive) plus extension letters and "fatal".
func ParseLevel(s string) (model.Level, bool) {
	switch strings.ToLower(s) {
	case "verbose", "v":
		return model.Verbose, true
	case "debug", "d":
		return model.Debug, true
	case "info", "i":
		return model.Info, true
	case "warn", "w":
		return model.Warn, true
	case "error", "e":
		return model.Error, true
	case "assert", "a", "fatal", "f":
		return model.Fatal, true
	}
	return 0, false
}

var durationRE = regexp.MustCompile(`^\d+[smhd]$`)

func parseAge(s string) (time.Duration, bool) {
	if !durationRE.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[s[len(s)-1]]
	if n > int64((1<<63-1)/unit) {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// combineTop applies AS's implicit top-level grouping.
func (p *parser) combineTop(tops []item) Node {
	if len(tops) == 0 {
		return nil
	}
	var nodes []Node
	if p.opts.JoinConsecutiveValues {
		for i := 0; i < len(tops); i++ {
			if !tops[i].isValue {
				nodes = append(nodes, tops[i].node)
				continue
			}
			j := i
			for j+1 < len(tops) && tops[j+1].isValue {
				j++
			}
			if j == i {
				nodes = append(nodes, tops[i].node)
				continue
			}
			var b strings.Builder
			for k := i; k <= j; k++ {
				b.WriteString(unescape(tops[k].tok.text))
				// whitespace between this literal and the next one, verbatim
				if k < j {
					b.WriteString(p.src[tops[k].tok.end:tops[k+1].tok.pos])
				}
			}
			v := strings.TrimSpace(b.String())
			nodes = append(nodes, &Str{Field: FImplicitLine, Value: v, lower: strings.ToLower(v)})
			i = j
		}
	} else {
		for _, t := range tops {
			nodes = append(nodes, t.node)
		}
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	if p.opts.SameKeyAND {
		return &And{Kids: nodes}
	}
	type group struct {
		key  string
		kids []Node
	}
	var groups []*group
	idx := map[string]*group{}
	for i, n := range nodes {
		k := implicitOrKey(n, i)
		g, ok := idx[k]
		if !ok {
			g = &group{key: k}
			idx[k] = g
			groups = append(groups, g)
		}
		g.kids = append(g.kids, n)
	}
	out := make([]Node, 0, len(groups))
	for _, g := range groups {
		if len(g.kids) == 1 {
			out = append(out, g.kids[0])
			continue
		}
		out = append(out, &Or{Kids: g.kids})
		names := make([]string, len(g.kids))
		for i, k := range g.kids {
			names[i] = k.String()
		}
		p.notes = append(p.notes, fmt.Sprintf("same-key terms are ORed: %s (use & for AND)", strings.Join(names, " | ")))
	}
	if len(out) == 1 {
		return out[0]
	}
	return &And{Kids: out}
}

// implicitOrKey mirrors LogcatFilterParser.getFieldForImplicitOr, extended
// so positive pid:/tid:/buffer: terms group per key as well.
func implicitOrKey(n Node, index int) string {
	switch x := n.(type) {
	case *Str:
		if !x.Neg && x.Field != FImplicitLine {
			return "field:" + x.Field.String()
		}
	case *Regex:
		if !x.Neg {
			return "field:" + x.Field.String()
		}
	case *Level:
		return "level"
	case *Age:
		return "age"
	case Crash, StackTrace:
		return "is"
	case *Num:
		if !x.Neg {
			return "ext:" + x.Key
		}
	case *Buffer:
		if !x.Neg {
			return "ext:buffer"
		}
	}
	return fmt.Sprintf("#%d", index)
}

// checkAdjacency rejects input that AS would silently split at ':' —
// e.g. "lvl:error" (unknown key), "Tag:foo" (uppercase key),
// "-level:warn" (non-negatable key) or "tag:a:b" (unquoted ':').
func (p *parser) checkAdjacency() *Error {
	for i := 0; i+1 < len(p.toks); i++ {
		a, b := p.toks[i], p.toks[i+1]
		if a.wsAft || b.kind != tkValue || !strings.HasPrefix(b.text, ":") {
			continue
		}
		if a.kind == tkValue && !a.quoted && isWordish(a.text) {
			word := a.text
			neg := strings.HasPrefix(word, "-")
			bare := strings.TrimPrefix(word, "-")
			op := ":"
			if strings.HasSuffix(bare, "=") || strings.HasSuffix(bare, "~") {
				op = bare[len(bare)-1:] + ":"
				bare = bare[:len(bare)-1]
			}
			low := strings.ToLower(bare)
			val := strings.TrimPrefix(b.text, ":")
			switch {
			case low != bare && isKnownKey(low):
				return errAt(p.src, a.pos, "keys are lowercase — use %s%s%s instead of %s%s",
					map[bool]string{true: "-"}[neg], low, op, word, ":")
			case neg && contains(regularKeys, bare):
				return errAt(p.src, a.pos, "%s: cannot be negated", bare).
					hint("%s", negationHint(bare, val))
			case contains(regularKeys, bare) || contains(extKeys, bare):
				return errAt(p.src, a.pos, "%s: does not support %q", bare, op).hint("Use %s:%s", bare, val)
			default:
				e := errAt(p.src, a.pos, "unknown key %q. Keys: %s", bare, strings.Join(AllKeys, " "))
				if s := suggestKey(low); s != "" {
					return e.hint("Did you mean: %s:%s   (to search the literal text, quote it: \"%s%s\")", s, val, word, b.text)
				}
				return e.hint("To search the literal text, quote it: \"%s%s\"", word, b.text)
			}
		}
		if a.kind == tkValue || a.kind == tkKValue {
			return errAt(p.src, b.pos, "unquoted value contains ':'").
				hint("Quote it or escape the colon: %q, or %s", unescape(a.text)+b.text, strings.ReplaceAll(a.text+b.text, ":", `\:`))
		}
	}
	return nil
}

func negationHint(key, val string) string {
	switch key {
	case "level":
		return "Android Studio has no -level:. Use level:<min> for a minimum level, or e.g. -message:… to exclude text."
	case "age":
		return "Use the from/to parameters for time windows."
	}
	return "Only tag message package process line pid tid buffer can be negated."
}

func isWordish(s string) bool {
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "="), "~")
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_') {
			return false
		}
	}
	return true
}

func isKnownKey(k string) bool { return contains(AllKeys, k) || k == "name" }

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

var keyAliases = map[string]string{
	"lvl": "level", "severity": "level", "priority": "level", "prio": "level", "lev": "level",
	"msg": "message", "text": "message", "pkg": "package", "app": "package", "appid": "package",
	"proc": "process", "t": "tag", "tags": "tag", "since": "age", "time": "age", "thread": "tid",
	"buf": "buffer", "uid": "package",
}

func suggestKey(k string) string {
	if a, ok := keyAliases[k]; ok {
		return a
	}
	if len(k) < 2 {
		return ""
	}
	best, bd := "", 3
	if len(k) == 2 {
		bd = 2
	}
	for _, c := range AllKeys {
		if d := editDistance(k, c); d < bd {
			best, bd = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(b)]
}

func suggestBalanced(s string) string {
	depth := 0
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				continue
			}
			depth--
		}
		b.WriteRune(r)
	}
	return b.String() + strings.Repeat(")", depth)
}

var firebaseRegex = "^(AppInstallOperation|AppInviteActivity|AppInviteAgent|AppInviteAnalytics|AppInviteLogger|BackgroundTask|ClassMapper|Connection|DataOperation|EventRaiser|FA|FirebaseAppIndex|FirebaseDatabase|FirebaseInstanceId|FirebaseMessaging|FirebaseRemoteConfig|NetworkRequest|Persistence|PersistentConnection|RepoOperation|RunLoop|StorageTask|SyncTree|Transaction|WebSocket)$"
