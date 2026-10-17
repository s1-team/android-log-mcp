package redact

import "strings"

// Prefilter for Text (#177). Almost every log message contains nothing
// sensitive, yet running ~24 regular expressions over it dominates the cost
// of a redacted export. scan makes ONE pass over the text and records cheap
// facts about it; every rule has a gate that is a *necessary* condition for
// its regexp to match (and for its validator to accept a match). A gate may
// only say "skip" when the rule provably cannot change the text, so the
// output is byte-identical to running every regexp (redact_ref_test.go
// keeps the old implementation and the differential tests prove it).
//
// Rules for writing a gate: it must be true whenever the regexp could match
// (false positives only cost time); case-insensitive rules use fold flags,
// which are all forced on when the text has a non-ASCII byte because Go's
// (?i) also folds U+017F (long s) to s and U+212A (Kelvin) to k.

// feat holds the facts scan found out about a text.
type feat struct {
	flags  uint32
	nd     int // number of ASCII digits
	maxRun int // longest run of ASCII digits
	colons int
	ddd    int // '.' with a digit on both sides (dotted-quad candidates)
}

const (
	// case-insensitive keywords
	fUsers uint32 = 1 << iota
	fAuthz
	fBearer
	fBasic
	fCookie
	fKey
	fSecretW
	fToken
	fPassw
	fCred
	fDevID // a device-id keyword (imei, imsi, serial, android_id ...) followed by ["']?\s*[:=]
	// case-sensitive literals and bytes
	fUnixHome // "/home/" or "/Users/"
	fEyJ
	fScheme     // "://"
	fSecretLit  // start of a well-known key format
	fAt         // '@'
	fPlus       // '+'
	fQuery      // '?' or '&'
	fEq         // '=' or ':'
	fPublicHost // a bare host candidate: token whose last label is a public TLD, first label not reverse-DNS
	fBankGroups // 4-digit groups separated by one space or dash (>= 3 groups)
	fPhoneCN    // 1[3-9]\d followed by two 4-digit groups (optional separators)
	fDoubleColon
	fNonASCII
)

const foldMask = fUsers | fAuthz | fBearer | fBasic | fCookie | fKey | fSecretW | fToken | fPassw | fCred | fDevID

func hasFold(s string, i int, w string) bool {
	if len(s)-i < len(w) {
		return false
	}
	for k := 0; k < len(w); k++ {
		c := s[i+k]
		if w[k] >= 'a' && w[k] <= 'z' {
			c |= 0x20
		}
		if c != w[k] {
			return false
		}
	}
	return true
}

func scan(s string) feat {
	var f feat
	var bk bankState
	run, runStart := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			f.nd++
			if run == 0 {
				runStart = i
			}
			run++
			if run > f.maxRun {
				f.maxRun = run
			}
			if c == '1' && f.flags&fPhoneCN == 0 && phoneCore(s, i) {
				f.flags |= fPhoneCN
			}
			continue
		}
		if run > 0 {
			endRun(&f, s, runStart, run, &bk)
			run = 0
		}
		if lc := c | 0x20; lc >= 'a' && lc <= 'z' {
			if f.flags&fDevID == 0 && devWords[lc-'a'] != nil && devKeyword(s, i, lc) {
				f.flags |= fDevID
			}
			switch lc {
			case 'a':
				switch {
				case hasFold(s, i, "authorization"):
					f.flags |= fAuthz
				}
				if c == 'A' && (strings.HasPrefix(s[i:], "AKIA") || strings.HasPrefix(s[i:], "ASIA") || strings.HasPrefix(s[i:], "AIza")) {
					f.flags |= fSecretLit
				}
			case 'b':
				if hasFold(s, i, "bearer") {
					f.flags |= fBearer
				} else if hasFold(s, i, "basic") {
					f.flags |= fBasic
				}
			case 'c':
				if hasFold(s, i, "cookie") {
					f.flags |= fCookie
				} else if hasFold(s, i, "credential") {
					f.flags |= fCred
				}
			case 'e':
				if c == 'e' && strings.HasPrefix(s[i:], "eyJ") {
					f.flags |= fEyJ
				}
			case 'g':
				if c == 'g' {
					if len(s)-i >= 4 && s[i+1] == 'h' && strings.IndexByte("pousr", s[i+2]) >= 0 && s[i+3] == '_' ||
						strings.HasPrefix(s[i:], "github_pat_") || strings.HasPrefix(s[i:], "glpat-") {
						f.flags |= fSecretLit
					}
				}
			case 'i':
			case 'k':
				if hasFold(s, i, "key") {
					f.flags |= fKey
				}
			case 'm':
			case 'n':
				if c == 'n' && strings.HasPrefix(s[i:], "npm_") {
					f.flags |= fSecretLit
				}
			case 'o':
			case 'p':
				if hasFold(s, i, "passw") {
					f.flags |= fPassw
				}
			case 's':
				if hasFold(s, i, "secret") {
					f.flags |= fSecretW
				}
				if c == 's' && strings.HasPrefix(s[i:], "sk-") {
					f.flags |= fSecretLit
				}
			case 't':
				if hasFold(s, i, "token") {
					f.flags |= fToken
				}
			case 'u':
				if hasFold(s, i, "users") {
					f.flags |= fUsers
				}
			case 'x':
				if c == 'x' && strings.HasPrefix(s[i:], "xox") {
					f.flags |= fSecretLit
				}
			}
			continue
		}
		switch c {
		case '.':
			if i > 0 && i+1 < len(s) {
				if p, n := s[i-1], s[i+1]; p >= '0' && p <= '9' && n >= '0' && n <= '9' {
					f.ddd++
				}
			}
			if f.flags&fPublicHost == 0 && i > 0 {
				if p := s[i-1]; p >= 'a' && p <= 'z' || p >= '0' && p <= '9' {
					j := i + 1
					for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
						j++
					}
					if j-i-1 >= 2 && j-i-1 <= 12 && publicTLDs[s[i+1:j]] && hostStart(s, i) {
						f.flags |= fPublicHost
					}
				}
			}
		case ':':
			f.colons++
			f.flags |= fEq
			if i+1 < len(s) && s[i+1] == ':' {
				f.flags |= fDoubleColon
			}
			if strings.HasPrefix(s[i:], "://") {
				f.flags |= fScheme
			}
		case '=':
			f.flags |= fEq
		case '@':
			f.flags |= fAt
		case '+':
			f.flags |= fPlus
		case '?', '&':
			f.flags |= fQuery
		case '/':
			if strings.HasPrefix(s[i:], "/home/") || strings.HasPrefix(s[i:], "/Users/") {
				f.flags |= fUnixHome
			}
		case '-':
			if strings.HasPrefix(s[i:], "-----BEGIN ") {
				f.flags |= fSecretLit
			}
		default:
			if c >= 0x80 {
				f.flags |= fNonASCII
			}
		}
	}
	if run > 0 {
		endRun(&f, s, runStart, run, &bk)
	}
	if f.flags&fNonASCII != 0 {
		f.flags |= foldMask
	}
	return f
}

func gateWinUsers(_ string, f *feat) bool { return f.flags&fUsers != 0 && f.colons > 0 }
func gateUsers(_ string, f *feat) bool    { return f.flags&fUsers != 0 }
func gateUnixHome(_ string, f *feat) bool { return f.flags&fUnixHome != 0 }
func gatePassword(_ string, f *feat) bool { return f.flags&fScheme != 0 && f.flags&fAt != 0 }
func gateAuthz(_ string, f *feat) bool    { return f.flags&fAuthz != 0 }
func gateBearer(_ string, f *feat) bool   { return f.flags&(fBearer|fBasic) != 0 }
func gateJWT(_ string, f *feat) bool      { return f.flags&fEyJ != 0 }
func gateCookie(_ string, f *feat) bool   { return f.flags&fCookie != 0 }
func gateSecret(_ string, f *feat) bool   { return f.flags&fSecretLit != 0 }
func gateCredential(_ string, f *feat) bool {
	return f.flags&fEq != 0 && f.flags&(fKey|fSecretW|fToken|fPassw|fCred) != 0
}
func gateDeviceID(_ string, f *feat) bool { return f.flags&fDevID != 0 }
func gateURLParam(_ string, f *feat) bool { return f.flags&fQuery != 0 && f.flags&fEq != 0 }
func gateEmail(_ string, f *feat) bool    { return f.flags&fAt != 0 }
func gateIDCard(_ string, f *feat) bool   { return f.maxRun >= 17 }

// bank_card: a bare run of 13-19 digits, or three/four groups of 4 digits.
func gateBankCard(_ string, f *feat) bool {
	return f.maxRun >= 13 || f.flags&fBankGroups != 0
}

func gatePhoneCN(_ string, f *feat) bool { return f.flags&fPhoneCN != 0 }
func gatePlus(_ string, f *feat) bool    { return f.flags&fPlus != 0 }
func gateMAC(_ string, f *feat) bool     { return f.colons >= 5 }
func gateIPv4(_ string, f *feat) bool    { return f.ddd >= 3 }

// ipv6: net.ParseIP accepts a hex/colon string only with a "::" or 8 groups.
func gateIPv6(_ string, f *feat) bool   { return f.flags&fDoubleColon != 0 || f.colons >= 7 }
func gateScheme(_ string, f *feat) bool { return f.flags&fScheme != 0 }

// bare hostnames are only accepted when the last label is a public TLD.
func gateBareHost(_ string, f *feat) bool { return f.flags&fPublicHost != 0 }

// homeGate: every literal path component of the home directory must occur in
// the text; the longest one is the cheapest to look for.
func homeGate(parts []string, caseInsensitive bool) func(string, *feat) bool {
	best := ""
	for _, p := range parts {
		if len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return nil
	}
	if !caseInsensitive {
		return func(s string, _ *feat) bool { return strings.Contains(s, best) }
	}
	for i := 0; i < len(best); i++ {
		if best[i] >= 0x80 {
			return nil // Unicode case folding: do not second-guess the regexp
		}
	}
	lower := strings.ToLower(best)
	return func(s string, f *feat) bool {
		return f.flags&fNonASCII != 0 || containsFold(s, lower)
	}
}

// containsFold: ASCII case-insensitive substring search, w lower-case ASCII.
func containsFold(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i]|0x20 == w[0]|0x20 && eqFold(s[i:i+len(w)], w) {
			return true
		}
	}
	return false
}

func eqFold(a, w string) bool {
	for k := 0; k < len(w); k++ {
		c := a[k]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != w[k] {
			return false
		}
	}
	return true
}

// devWords: device-id key names by first letter (all forms of the rule's
// alternation, lower-case).
var devWords = func() (t [26][]string) {
	for _, w := range []string{"imei", "imsi", "iccid", "meid", "esn", "serial", "serialno", "serialnumber", "serial_number",
		"androidid", "android_id", "ssaid", "advertisingid", "advertising_id", "gaid", "oaid"} {
		t[w[0]-'a'] = append(t[w[0]-'a'], w)
	}
	return
}()

// devKeyword: a key name at s[i] followed by `["']?\s*[:=]`.
func devKeyword(s string, i int, lc byte) bool {
	for _, w := range devWords[lc-'a'] {
		if !hasFold(s, i, w) {
			continue
		}
		j := i + len(w)
		if j < len(s) && (s[j] == '"' || s[j] == '\'') {
			j++
		}
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\f' || s[j] == '\r') {
			j++
		}
		if j < len(s) && (s[j] == ':' || s[j] == '=') {
			return true
		}
	}
	return false
}

func isDig(c byte) bool { return c >= '0' && c <= '9' }

// phoneCore: s[i]=='1' starts 1[3-9]\d(?:[ -]?\d{4}){2}.
func phoneCore(s string, i int) bool {
	if i+2 >= len(s) || s[i+1] < '3' || s[i+1] > '9' || !isDig(s[i+2]) {
		return false
	}
	j := i + 3
	for g := 0; g < 2; g++ {
		if j < len(s) && (s[j] == ' ' || s[j] == '-') {
			j++
		}
		if j+4 > len(s) || !isDig(s[j]) || !isDig(s[j+1]) || !isDig(s[j+2]) || !isDig(s[j+3]) {
			return false
		}
		j += 4
	}
	return true
}

// bankState tracks \d{4}(?:[ -]\d{4}){2,3}: runs of digits joined by one
// separator, first and last >= 4 digits, the ones in between exactly 4.
type bankState struct {
	prevEnd int // index of the byte after the previous run
	have    bool
	s1      bool // previous run has >= 4 digits
	mid     bool // previous run can be a middle group preceded by a valid first group
}

func endRun(f *feat, s string, start, n int, b *bankState) {
	adj := b.have && start >= 1 && b.prevEnd == start-1 && (s[start-1] == ' ' || s[start-1] == '-')
	s1 := n >= 4
	s2 := adj && b.s1 && n >= 4
	if adj && b.mid && n >= 4 {
		f.flags |= fBankGroups
	}
	*b = bankState{prevEnd: start + n, have: true, s1: s1, mid: s2 && n == 4}
}

// hostStart: the host-like token ([a-z0-9.-]+) containing the dot at i is
// one bareHost could accept: its first label is not a reverse-DNS root or a
// TLD, and the byte before the token is none of those bareHost rejects.
func hostStart(s string, i int) bool {
	t := i
	for t > 0 {
		c := s[t-1]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' {
			t--
			continue
		}
		break
	}
	if t > 0 {
		switch p := s[t-1]; {
		case isAlnum(p) || p == '/' || p == '@' || p == '$' || p == '\\' || p == '#':
			return false
		}
	}
	if c := s[t]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false // a leading '.' or '-': the match would start after it
	}
	e := t
	for e < i && s[e] != '.' {
		e++
	}
	first := s[t:e]
	return !reverseDNSRoots[first] && !publicTLDs[first]
}
