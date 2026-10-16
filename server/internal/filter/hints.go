package filter

import "strings"

// Hint is a filter-syntax hint: the query parsed fine but a common mistake
// would have given a valid, unintended meaning (issue #86). Hints never change
// the parse tree. Fix, when set, is the whole filter rewritten as it was
// probably meant (single quotes only, so it pastes into JSON on any shell).
type Hint struct {
	Kind string // or_word, multi_word_value, mixed_or
	Msg  string
	Fix  string
}

// Hint kinds.
const (
	HintOrWord    = "or_word"
	HintMultiWord = "multi_word_value"
	HintMixedOr   = "mixed_or"
)

// maxHints caps the hints of one parse; each kind fires at most once.
const maxHints = 2

// hints inspects the token stream (never the tree) for the three silent-misuse
// patterns. Order is the priority when the cap applies.
func (p *parser) hints(root Node, topCount int) []Hint {
	var out []Hint
	add := func(h *Hint) {
		if h != nil && len(out) < maxHints {
			out = append(out, *h)
		}
	}
	add(p.hintOrWord())
	add(p.hintMixedOr(root, topCount))
	add(p.hintMultiWord())
	return out
}

func isBareWord(t token, words ...string) bool {
	if t.kind != tkValue || t.quoted {
		return false
	}
	for _, w := range words {
		if t.text == w {
			return true
		}
	}
	return false
}

// hintOrWord: an unquoted, standalone, uppercase OR / AND term.
func (p *parser) hintOrWord() *Hint {
	for _, t := range p.toks {
		if !isBareWord(t, "OR", "AND") {
			continue
		}
		op := "|"
		if t.text == "AND" {
			op = "&"
		}
		return &Hint{
			Kind: HintOrWord,
			Msg:  t.text + " is a message keyword, not an operator; write " + op + " between terms, or quote it: '" + t.text + "'",
			Fix:  p.src[:t.pos] + op + p.src[t.end:],
		}
	}
	return nil
}

// hintMultiWord: a text key (not regex) with an unquoted value, directly
// followed by a bare implicit term. Plain implicit terms alone never fire.
func (p *parser) hintMultiWord() *Hint {
	for i := 0; i+2 < len(p.toks); i++ {
		k, v, n := p.toks[i], p.toks[i+1], p.toks[i+2]
		if k.kind != tkStrKey || v.kind != tkKValue || v.quoted || n.kind != tkValue || n.quoted {
			continue
		}
		if isBareWord(n, "OR", "AND", "or", "and", "&&", "||") {
			continue
		}
		phrase := p.src[v.pos:n.end]
		h := &Hint{
			Kind: HintMultiWord,
			Msg:  "'" + n.text + "' is a separate term (AND), not part of " + k.text + v.text + "; to match the phrase quote it: " + k.text + "'" + phrase + "'",
		}
		if !strings.Contains(phrase, "'") {
			h.Fix = p.src[:v.pos] + "'" + phrase + "'" + p.src[n.end:]
		}
		return h
	}
	return nil
}

// hintMixedOr: an unparenthesized top-level | together with implicit AND.
func (p *parser) hintMixedOr(root Node, topCount int) *Hint {
	if topCount < 2 {
		return nil
	}
	depth, hasOr := 0, false
	for _, t := range p.toks {
		switch t.kind {
		case tkLParen:
			depth++
		case tkRParen:
			depth--
		case tkOr:
			if depth == 0 {
				hasOr = true
			}
		}
	}
	if !hasOr {
		return nil
	}
	return &Hint{
		Kind: HintMixedOr,
		Msg:  "| binds tighter than a space (implicit AND): parsed as " + strings.ReplaceAll(root.String(), `"`, "'") + "; to OR whole groups wrap each side in parentheses joined with &",
		Fix:  p.groupFix(),
	}
}

// groupFix rewrites the top level as (left group) | (right group): units are
// joined with & inside a group (parentheses do not allow a bare space).
func (p *parser) groupFix() string {
	var groups [][]string
	cur := []string{}
	for i := 0; i < len(p.toks); i++ {
		t := p.toks[i]
		var unit string
		switch t.kind {
		case tkEOF:
			continue
		case tkOr:
			groups = append(groups, cur)
			cur = []string{}
			continue
		case tkAnd:
			continue
		case tkStrKey, tkRegexKey, tkKey:
			if i+1 >= len(p.toks) {
				return ""
			}
			unit = p.src[t.pos:p.toks[i+1].end]
			i++
		case tkLParen:
			d, j := 0, i
			for ; j < len(p.toks); j++ {
				if p.toks[j].kind == tkLParen {
					d++
				} else if p.toks[j].kind == tkRParen {
					d--
					if d == 0 {
						break
					}
				}
			}
			if j >= len(p.toks) {
				return ""
			}
			unit = p.src[t.pos:p.toks[j].end]
			i = j
		default:
			unit = p.src[t.pos:t.end]
		}
		cur = append(cur, unit)
	}
	groups = append(groups, cur)
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		if len(g) == 0 {
			return ""
		}
		s := strings.Join(g, " & ")
		if len(g) > 1 {
			s = "(" + s + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}
