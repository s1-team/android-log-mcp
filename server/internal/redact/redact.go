// Package redact replaces sensitive values in free text (log messages,
// crash bodies, DropBox entries) with stable placeholders
// "[REDACTED:<type>#<n>]".
//
// Within one Redactor the same original value always maps to the same
// number, so occurrences can still be correlated. Numbers are assigned via
// an HMAC with a random per-Redactor key that is never stored or exposed,
// and only HMACs (not values) are kept in memory: placeholders cannot be
// reversed.
//
// Rules are conservative and validated (Luhn for cards, GB 11643 checksum
// and birth date for CN ID numbers, octet ranges for IPs, explicit token
// boundaries) so that log structure — timestamps, pid/tid, levels, tags,
// package/process names, hex addresses, stack frames, version numbers — is
// never altered. Callers must only pass message-like text, not headers.
package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Options configures a Redactor.
type Options struct {
	Disabled      bool     // --no-redact
	KeepPrivateIP bool     // keep RFC 1918 / link-local / ULA addresses
	KeepDomains   []string // --keep-domain: hostnames (and their subdomains) left as they are
	Extra         []string // --redact-extra regexes (group 1 if present, else the whole match)
	// Home is the home directory of the user running the tool: every
	// occurrence (either slash type; case-insensitive for a Windows path) is
	// replaced as a whole, so a home that is not under /home, /Users or
	// C:\Users is covered too. Empty: only the generic user-path rules apply.
	Home string
}

// Redactor is safe for concurrent use.
type Redactor struct {
	opt   Options
	key   []byte
	rules []rule

	mu     sync.Mutex
	ids    map[string]map[string]int // type → hmac → n
	hits   map[string]int
	nextID map[string]int
}

type rule struct {
	typ   string
	re    *regexp.Regexp
	group int // submatch to replace (0 = whole match)
	// gate is a cheap necessary condition for re to match anywhere in s
	// (nil = always run the regexp). It may only return false when the
	// regexp provably has no match; see scan.go.
	gate func(s string, f *feat) bool
	// ok validates the candidate value v found at s[start:end] (bounds of the replaced group).
	ok func(s string, start, end int, v string) bool
}

// Placeholder prefix; text that already contains it is never re-redacted.
const prefix = "[REDACTED:"

// New builds a Redactor with a fresh random key.
func New(o Options) (*Redactor, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	r := &Redactor{opt: o, key: key, ids: map[string]map[string]int{}, hits: map[string]int{}, nextID: map[string]int{}}
	for i, x := range o.Extra {
		re, err := regexp.Compile(x)
		if err != nil {
			return nil, fmt.Errorf("--redact-extra #%d %q: %v", i+1, x, err)
		}
		g := 0
		if re.NumSubexp() > 0 {
			g = 1
		}
		r.rules = append(r.rules, rule{typ: "custom", re: re, group: g})
	}
	// the whole home directory first, then the generic user-path rules
	if hr := homeRule(o.Home); hr != nil {
		r.rules = append(r.rules, *hr)
	}
	r.rules = append(r.rules, builtin(o)...)
	return r, nil
}

// Mask returns the placeholder for a value that the caller knows is
// sensitive (for example the serial in `adb devices -l`), counted like a
// rule hit: the same value gets the same number as in the rest of the text.
func (r *Redactor) Mask(typ, v string) string {
	if r.opt.Disabled || v == "" {
		return v
	}
	return r.placeholder(typ, v)
}

// homeRule redacts the literal home directory. The directory separators match
// either slash type, repeated (a JSON-escaped path has "\\\\").
func homeRule(home string) *rule {
	h := strings.TrimRight(home, `/\`)
	if len(h) < 3 {
		return nil
	}
	parts := regexp.MustCompile(`[\\/]+`).Split(h, -1)
	raw := append([]string(nil), parts...)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := strings.Join(parts, `[\\/]+`)
	if strings.Contains(h, `\`) || strings.Contains(h, ":") {
		expr = "(?i)" + expr // a Windows path is case-insensitive
	}
	re, err := regexp.Compile(`(?:^|[^A-Za-z0-9_.-])(` + expr + `)(?:[\\/]|[^A-Za-z0-9_.-]|$)`)
	if err != nil {
		return nil
	}
	return &rule{typ: "home_dir", re: re, group: 1, gate: homeGate(raw, strings.HasPrefix(expr, "(?i)"))}
}

// Enabled reports whether redaction is on.
func (r *Redactor) Enabled() bool { return !r.opt.Disabled }

// Stats returns hits per rule type (copy).
func (r *Redactor) Stats() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[string]int{}
	for k, v := range r.hits {
		m[k] = v
	}
	return m
}

// Unique returns the number of distinct values per rule type.
func (r *Redactor) Unique() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[string]int{}
	for k, v := range r.ids {
		m[k] = len(v)
	}
	return m
}

// Types lists the built-in rule types (for documentation/manifest).
func Types() []string {
	seen := map[string]bool{}
	var out []string
	for _, ru := range builtin(Options{}) {
		if !seen[ru.typ] {
			seen[ru.typ] = true
			out = append(out, ru.typ)
		}
	}
	sort.Strings(out)
	return out
}

// Text redacts s.
func (r *Redactor) Text(s string) string {
	if r.opt.Disabled || s == "" {
		return s
	}
	f := scan(s)
	for i := range r.rules {
		ru := &r.rules[i]
		if ru.gate != nil && !ru.gate(s, &f) {
			continue
		}
		if ns, changed := r.apply(ru, s); changed {
			s = ns
			f = scan(s) // later gates must see the placeholders just inserted
		}
	}
	return s
}

func (r *Redactor) placeholder(typ, v string) string {
	m := hmac.New(sha256.New, r.key)
	m.Write([]byte(typ))
	m.Write([]byte{0})
	m.Write([]byte(v))
	h := hex.EncodeToString(m.Sum(nil))
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := r.ids[typ]
	if ids == nil {
		ids = map[string]int{}
		r.ids[typ] = ids
	}
	n, ok := ids[h]
	if !ok {
		r.nextID[typ]++
		n = r.nextID[typ]
		ids[h] = n
	}
	r.hits[typ]++
	return prefix + typ + "#" + strconv.Itoa(n) + "]"
}

func (r *Redactor) apply(ru *rule, s string) (string, bool) {
	ms := ru.re.FindAllStringSubmatchIndex(s, -1)
	if len(ms) == 0 {
		return s, false
	}
	// insidePlaceholder can only be true when s contains a placeholder
	// prefix at all; without this check it is a backwards scan per match.
	hasPrefix := strings.Contains(s, prefix)
	var b strings.Builder
	b.Grow(len(s) + 16*len(ms))
	last := 0
	for _, m := range ms {
		a, e := m[2*ru.group], m[2*ru.group+1]
		if a < 0 || a < last {
			continue
		}
		v := s[a:e]
		if v == "" || strings.Contains(v, prefix) || hasPrefix && insidePlaceholder(s, a) {
			continue
		}
		if ru.ok != nil && !ru.ok(s, a, e, v) {
			continue
		}
		b.WriteString(s[last:a])
		b.WriteString(r.placeholder(ru.typ, v))
		last = e
	}
	if last == 0 {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// insidePlaceholder reports whether position i lies inside "[REDACTED:...]".
func insidePlaceholder(s string, i int) bool {
	j := strings.LastIndex(s[:i], prefix)
	if j < 0 {
		return false
	}
	k := strings.IndexByte(s[j:], ']')
	return k < 0 || j+k >= i
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// numBoundary: a number token must not continue a word, a hex literal or a
// decimal (before: alnum, '.', "0x"; after: alnum or ".<digit>").
func numBoundary(s string, a, e int) bool {
	if a > 0 {
		p := s[a-1]
		if isAlnum(p) || p == '.' || p == '/' && a > 1 && isDigit(s[a-2]) {
			return false
		}
	}
	if e < len(s) {
		n := s[e]
		if isAlnum(n) {
			return false
		}
		if n == '.' && e+1 < len(s) && isDigit(s[e+1]) {
			return false
		}
	}
	return true
}

func digitsOnly(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if isDigit(v[i]) {
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

func luhn(d string) bool {
	sum := 0
	for i := 0; i < len(d); i++ {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return len(d) > 0 && sum%10 == 0
}

// validCNID checks a GB 11643 18-digit resident ID: plausible birth date and checksum.
func validCNID(id string) bool {
	if len(id) != 18 {
		return false
	}
	y, _ := strconv.Atoi(id[6:10])
	mo, _ := strconv.Atoi(id[10:12])
	d, _ := strconv.Atoi(id[12:14])
	if y < 1900 || y > 2099 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return false
	}
	w := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		if !isDigit(id[i]) {
			return false
		}
		sum += int(id[i]-'0') * w[i]
	}
	c := "10X98765432"[sum%11]
	last := id[17]
	if last == 'x' {
		last = 'X'
	}
	return last == c
}

// cardIIN: first digits of real card networks (Visa 4, Mastercard 51–55 / 2221–2720,
// Amex 34/37, Diners/JCB 30/35/36/38, Discover/UnionPay 6). Excludes epoch
// timestamps (1…) and zero-padded offsets (0…).
func cardIIN(d string) bool {
	switch d[0] {
	case '3', '4', '5', '6':
		return true
	case '2':
		p, _ := strconv.Atoi(d[:4])
		return p >= 2221 && p <= 2720
	}
	return false
}

var (
	reObjRef  = regexp.MustCompile(`^[A-Za-z_$][\w.$]*(?:@[0-9a-fA-F]+|\{[0-9a-fA-F]*)`)
	reBoolish = regexp.MustCompile(`(?i)^(?:null|nil|none|undefined|true|false|yes|no|\*+|x+|\d{1,6}|<[^>]*>|\[[^\]]*\]|\.\.\.)$`)
)

func credentialValue(v string) bool {
	u := strings.Trim(v, `"'`)
	if u == "" || reBoolish.MatchString(u) || reObjRef.MatchString(u) {
		return false
	}
	return true
}

// userName is the user-name segment of a home path: stops at a separator, a
// quote or an existing placeholder.
const userName = `[^\\/\s"'<>|:*?\[\]]+`

func builtin(o Options) []rule {
	keepIP := o.KeepPrivateIP
	return []rule{
		// user names inside home paths: /home/<user>, /Users/<user>,
		// C:\Users\<user> (either slash type, JSON-escaped backslashes, any
		// case for the Windows form; a Windows name may contain spaces)
		{typ: "username", gate: gateWinUsers, re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])[A-Za-z]:[\\/]+Users[\\/]+(` + userName + `(?: ` + userName + `)+)[\\/]`), group: 1},
		{typ: "username", gate: gateUsers, re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:[A-Za-z]:[\\/]+|\\+)Users[\\/]+(` + userName + `)`), group: 1},
		{typ: "username", gate: gateUnixHome, re: regexp.MustCompile(`/(?:home|Users)/(` + userName + `)`), group: 1},
		// credentials embedded in URLs: scheme://user:password@host
		{typ: "password", gate: gatePassword, re: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`), group: 1},
		// Authorization headers (value, with or without scheme)
		{typ: "token", gate: gateAuthz, re: regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|digest|token|negotiate)\s+)?([A-Za-z0-9._~+/=-]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return tokenLike(v) }},
		{typ: "token", gate: gateBearer, re: regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return tokenLike(v) }},
		{typ: "jwt", gate: gateJWT, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)},
		// Cookie / Set-Cookie header values (whole value: several name=value pairs)
		{typ: "cookie", gate: gateCookie, re: regexp.MustCompile(`(?i)\b(?:set-)?cookie["']?\s*[:=]\s*["']?([^\s"'][^\n"']*)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return strings.Contains(v, "=") && credentialValue(v) }},
		// well-known key formats
		{typ: "secret", gate: gateSecret, re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{50,}|npm_[A-Za-z0-9]{36}|sk-(?:proj-|ant-|live-|test-)?[A-Za-z0-9_-]{20,}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|xox[abposr]-[A-Za-z0-9-]{10,}|glpat-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)},
		// key=value / "key": "value" with a sensitive key name (also URL query params)
		{typ: "credential", gate: gateCredential, re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])["']?[A-Za-z0-9_.-]*?(?:api[_-]?key|apikey|secret|token|passw(?:or)?d|access[_-]?key|private[_-]?key|auth[_-]?key|credentials?)s?["']?\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s,;&"'}\])]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool {
				if !credentialValue(v) {
					return false
				}
				// "removeWindowToken: Attempted to …": after a bare colon only an opaque value counts
				if v[0] != '"' && v[0] != '\'' && strings.HasSuffix(strings.TrimRight(s[:a], " "), ":") {
					return tokenLike(v)
				}
				return true
			}},
		// hardware / subscriber identifiers given as key=value (IMEI, IMSI, ICCID, MEID, serial, Android ID)
		{typ: "device_id", gate: gateDeviceID, re: regexp.MustCompile(`(?i)\b(?:imei|imsi|iccid|meid|esn|serial(?:no|number|_number)?|android_?id|ssaid|advertising_?id|gaid|oaid)["']?\s*[:=]\s*["']?([0-9A-Za-z-]{8,40})`), group: 1,
			ok: func(s string, a, e int, v string) bool { return credentialValue(v) && !strings.EqualFold(v, "unknown") }},
		// other sensitive URL query parameters
		{typ: "url_param", gate: gateURLParam, re: regexp.MustCompile(`(?i)[?&](?:sig|signature|code|key|auth|ticket|sid|session|otp|nonce|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&#\s"'<>]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return credentialValue(v) }},
		{typ: "email", gate: gateEmail, re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,24}\b`),
			ok: func(s string, a, e int, v string) bool {
				if a > 0 && (isAlnum(s[a-1]) || s[a-1] == '.' || s[a-1] == '/' || s[a-1] == '@') {
					return false // part of a word or a file path
				}
				// versioned HIDL/AIDL names: android.hardware.sensors@1.0.so, foo@2.1-impl
				dom := v[strings.IndexByte(v, '@')+1:]
				first, _, _ := strings.Cut(dom, ".")
				return strings.Trim(first, "0123456789") != ""
			}},
		{typ: "id_card", gate: gateIDCard, re: regexp.MustCompile(`\d{17}[\dXx]`),
			ok: func(s string, a, e int, v string) bool {
				return numBoundary(s, a, e) && validCNID(v) && !reNumericUnit.MatchString(strings.ToLower(s[e:min(len(s), e+8)]))
			}},
		{typ: "bank_card", gate: gateBankCard, re: regexp.MustCompile(`\d{4}(?:[ -]\d{4}){2,3}(?:[ -]\d{1,3})?|\d{13,19}`),
			ok: func(s string, a, e int, v string) bool {
				d := digitsOnly(v)
				if !numBoundary(s, a, e) || len(d) < 13 || len(d) > 19 || !cardIIN(d) || !luhn(d) {
					return false
				}
				if len(d) == len(v) { // a bare digit run: much weaker evidence than "4111 1111 1111 1111"
					if len(d) != 15 && len(d) != 16 && len(d) != 19 || hexDumpLine(s, a, e) || numericContext(s, a, e) {
						return false
					}
					if k := keyBefore(s, a); k != "" && !reCardKey.MatchString(k) {
						return false // "IntendedVsync=6355…": a counter in a key=value dump
					}
				}
				return true
			}},
		{typ: "phone", gate: gatePhoneCN, re: regexp.MustCompile(`(?:\+?86[ -]?)?1[3-9]\d(?:[ -]?\d{4}){2}`),
			ok: func(s string, a, e int, v string) bool {
				return numBoundary(s, a, e) && !(a > 0 && s[a-1] == '+') && !numericContext(s, a, e) && !numberColumn(s, a, e)
			}},
		{typ: "phone", gate: gatePlus, re: regexp.MustCompile(`\+[1-9]\d{0,2}[ -]?(?:\(\d{1,4}\)[ -]?)?\d{2,4}(?:[ -]?\d{2,4}){1,3}`),
			ok: func(s string, a, e int, v string) bool {
				n := len(digitsOnly(v))
				if len(v) == n+1 && n < 10 { // compact "+NNNN…" needs E.164 length; "+28800000" is an offset
					return false
				}
				return numBoundary(s, a, e) && n >= 8 && n <= 15 && !numericContext(s, a, e)
			}},
		{typ: "mac", gate: gateMAC, re: regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`),
			ok: func(s string, a, e int, v string) bool {
				if e < len(s) && s[e] == ':' {
					return false
				}
				l := strings.ToLower(v)
				return l != "00:00:00:00:00:00" && l != "02:00:00:00:00:00" && l != "ff:ff:ff:ff:ff:ff"
			}},
		{typ: "ipv4", gate: gateIPv4, re: regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`),
			ok: func(s string, a, e int, v string) bool {
				if !numBoundary(s, a, e) || versionContext(s, a) {
					return false
				}
				ip := net.ParseIP(v)
				if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || v == "255.255.255.255" {
					return false
				}
				return !(keepIP && (ip.IsPrivate() || ip.IsLinkLocalUnicast()))
			}},
		{typ: "ipv6", gate: gateIPv6, re: regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}`),
			ok: func(s string, a, e int, v string) bool {
				if len(v) < 6 || a > 0 && (isAlnum(s[a-1]) || s[a-1] == ':') || e < len(s) && (isAlnum(s[e]) || s[e] == ':') {
					return false
				}
				ip := net.ParseIP(v)
				if ip == nil || ip.To4() != nil || ip.IsLoopback() || ip.IsUnspecified() {
					return false
				}
				return !(keepIP && (ip.IsPrivate() || ip.IsLinkLocalUnicast()))
			}},
		// hostnames — last, so that emails, URL passwords/params and IPs are already handled.
		// 1) the host of a URL (any dotted, non-IP host; scheme, port and path are kept)
		{typ: "domain", gate: gateScheme, re: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://(?:[^\s/@?]*@)?([^\s/:?#\[\]@"'<>\\]+)`), group: 2,
			ok: func(s string, a, e int, v string) bool {
				m := reScheme.FindStringSubmatch(s[:a])
				if m == nil || nonNetSchemes[strings.ToLower(m[1])] {
					return false
				}
				return urlHost(v) && !packageAuthority(v) && !keptDomain(o.KeepDomains, v)
			}},
		// 2) bare hostnames in text: lower-case labels ending in a common public TLD,
		//    not shaped like a reverse-DNS Java/Android name
		{typ: "domain", gate: gateBareHost, re: regexp.MustCompile(`(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,12}`),
			ok: func(s string, a, e int, v string) bool { return bareHost(s, a, e, v) && !keptDomain(o.KeepDomains, v) }},
	}
}

// reScheme finds the scheme of the URL whose host starts where the text ends.
var reScheme = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*)://(?:[^\s/@?]*@)?$`)

// nonNetSchemes have an authority that is not a network host
// (content://com.android.providers.media/…, file:///…).
var nonNetSchemes = map[string]bool{"content": true, "file": true, "android.resource": true, "android-app": true, "jar": true, "data": true, "intent": true, "market": true, "package": true, "res": true, "asset": true, "chrome": true, "about": true}

func urlHost(v string) bool {
	if !strings.Contains(v, ".") || strings.Contains(v, prefix) || net.ParseIP(v) != nil {
		return false
	}
	hasAlpha := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			hasAlpha = true
		case isDigit(c) || c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return hasAlpha && !strings.HasPrefix(v, ".") && !strings.HasSuffix(v, ".")
}

// packageAuthority: "app://com.google.android.apps.nexuslauncher:10108" —
// an application id used as URL authority, not a network host.
func packageAuthority(v string) bool {
	labels := strings.Split(strings.ToLower(v), ".")
	return reverseDNSRoots[labels[0]] && !publicTLDs[labels[len(labels)-1]]
}

func keptDomain(keep []string, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, k := range keep {
		k = strings.ToLower(strings.Trim(k, ". "))
		if k != "" && (h == k || strings.HasSuffix(h, "."+k)) {
			return true
		}
	}
	return false
}

// publicTLDs: TLDs accepted for bare hostnames. Deliberately not the full IANA
// list: TLDs that are also common file extensions, words or property/field
// names (so, md, py, sh, cc, mk, pl, rs, ar, id, in, it, no, is, at, be, to, do,
// name, media, network, data, zone, mobile, top, site, online, store, shop,
// cloud, blog, wiki, …) are left out, so
// "libc.so" or "persist.sys.user.id" are never taken for hosts. Hosts on those
// TLDs are still redacted inside URLs.
var publicTLDs = toSet(`com net org edu gov mil int io co ai app dev me tv info biz xyz tech
cn uk de jp fr ru us ca au br kr tw hk mo sg nl es se ch eu nz za mx my th vn ph tr dk fi ie il pt cz gr hu ro sk ua
ir pk ng eg sa ae cl pe ve kz by lt lv ee si hr bg qa kw lk bd np ke`)

// reverseDNSRoots: first labels of Java/Android/Kotlin names (com.example.app,
// android.app.ActivityThread, java.lang.RuntimeException, jp.co.foo.app …).
// A first label that is itself a TLD means reverse-DNS, not a hostname.
var reverseDNSRoots = toSet(`android androidx java javax kotlin kotlinx dalvik libcore sun jdk junit org com net edu gov io dev app me
okhttp3 okio retrofit2 dagger vendor system persist ro sys debug ctl init service`)

func toSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

func bareHost(s string, a, e int, v string) bool {
	if a > 0 {
		switch p := s[a-1]; {
		case isAlnum(p) || p == '.' || p == '-' || p == '/' || p == '@' || p == '$' || p == '\\' || p == '#':
			return false // part of a word, a path, a library@version, an inner class
		}
	}
	if e < len(s) {
		switch n := s[e]; {
		case isAlnum(n) || n == '-' || n == '(' || n == '$' || n == '@' || n == '[':
			return false // method call, inner class, longer identifier
		case n == '.' && e+1 < len(s) && isAlnum(s[e+1]):
			return false // the regex stopped at an upper-case/underscore label: an identifier
		}
	}
	labels := strings.Split(v, ".")
	if len(labels) < 2 || !publicTLDs[labels[len(labels)-1]] || publicTLDs[labels[0]] || reverseDNSRoots[labels[0]] {
		return false
	}
	for _, l := range labels {
		if strings.Trim(l, "0123456789") == "" {
			return false // all-numeric label: a version / IP-like
		}
	}
	return true
}

// tokenLike: an opaque credential, not an English word ("Basic information").
func tokenLike(v string) bool {
	if strings.Contains(v, prefix) || len(v) < 8 {
		return false
	}
	hasDigit, hasOther := false, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if isDigit(c) {
			hasDigit = true
		} else if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			hasOther = true
		}
	}
	return hasDigit || hasOther || len(v) >= 24
}

var (
	reHex16 = regexp.MustCompile(`^(?:0x)?[0-9a-fA-F]{16}$`) // 64-bit register / address column
	reHex8  = regexp.MustCompile(`^(?:0x)?[0-9a-fA-F]{8}$`)
	reFrame = regexp.MustCompile(`#\d{2,} pc `)
	// whole-word keys, or suffixes of camelCase/snake_case keys ("totalBytes", "elapsed_ms")
	reNumericKey  = regexp.MustCompile(`(?i)^(?:ms|ns|us|sec|secs|len|seq|uid|pid|tid|id|ino|addr|code|free|used|avail)$|(?i:time|timestamp|millis|nanos|seconds|size|bytes|length|count|total|duration|elapsed|uptime|offset|version|versioncode|hash|serial|inode|address)$|(?:[a-z_](?:Ms|Ns|Us|Sec|Id|Len|Seq)|_(?i:ms|ns|us|sec|id|len|seq))$`)
	reNumericUnit = regexp.MustCompile(`^\s?(?:ms|ns|us|µs|s|sec|secs|bytes|b|kb|mb|gb|kib|mib|gib)\b`)
	reCardKey     = regexp.MustCompile(`(?i)card|pan|acct|account|^cc|number|^no$|卡`)
	reVersionKey  = regexp.MustCompile(`(?i)^(?:version|versionname|ver|build|release|sdk|v|radio|baseband|bootloader|kernel|firmware|fw)$|(?i:version|versionname)$`)
)

// lineAround returns the physical line containing s[a:e].
func lineAround(s string, a, e int) string {
	i := strings.LastIndexByte(s[:a], '\n') + 1
	j := strings.IndexByte(s[e:], '\n')
	if j < 0 {
		return s[i:]
	}
	return s[i : e+j]
}

// hexDumpLine: a bare digit run inside a register/memory dump or a native
// stack frame (other fixed-width hex columns on the same line) is an address,
// not a card number.
func hexDumpLine(s string, a, e int) bool {
	ln := lineAround(s, a, e)
	if reFrame.MatchString(ln) {
		return true
	}
	v := s[a:e]
	n := 0
	for _, f := range strings.Fields(ln) {
		if f != v && (reHex16.MatchString(f) || reHex8.MatchString(f) && strings.ContainsAny(strings.ToLower(f), "abcdef")) {
			n++
		}
	}
	return n > 0
}

// prevWord returns the identifier just before position a, skipping
// separators such as "=", ":" and spaces ("bytes=", "elapsed: ").
func prevWord(s string, a int) string {
	i := a
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '=' || s[i-1] == ':' || s[i-1] == '"' || s[i-1] == '\'') {
		i--
		if a-i > 4 {
			break
		}
	}
	j := i
	for j > 0 && (isAlnum(s[j-1]) || s[j-1] == '-') {
		j--
	}
	return s[j:i]
}

// numericContext: the number is the value of a counter/size/time/id field or
// carries a unit ("bytes=13958643712", "elapsed 15000000000 ns").
func numericContext(s string, a, e int) bool {
	if w := prevWord(s, a); w != "" && reNumericKey.MatchString(w) {
		return true
	}
	return reNumericUnit.MatchString(strings.ToLower(s[e:min(len(s), e+8)]))
}

// versionContext: a dotted quad that is a version ("version 1.2.3.4",
// "Chrome/83.0.41.10"), not an address. "//1.2.3.4" (URL host) is an address.
func versionContext(s string, a int) bool {
	if a > 1 && s[a-1] == '/' && isAlnum(s[a-2]) {
		// "Chrome/83.0.41.10" is a product version; "www.google.com/1.2.3.4"
		// (Java InetAddress.toString) and "http://1.2.3.4" are addresses
		i := a - 1
		for i > 0 && (isAlnum(s[i-1]) || s[i-1] == '.' || s[i-1] == '-') {
			i--
		}
		if !strings.Contains(s[i:a-1], ".") {
			return true
		}
	}
	if a > 0 && (s[a-1] == 'v' || s[a-1] == 'V') {
		return true
	}
	w := prevWord(s, a)
	return w != "" && reVersionKey.MatchString(w)
}

// keyBefore returns the key of a "key=value" / "key: value" pair whose value
// starts at a ("" when the value is not preceded by such a key).
func keyBefore(s string, a int) string {
	i := a
	for i > 0 && (s[i-1] == '"' || s[i-1] == '\'') {
		i--
	}
	j := i
	for j > 0 && s[j-1] == ' ' {
		j--
	}
	if j == 0 || (s[j-1] != '=' && s[j-1] != ':') {
		return ""
	}
	j--
	for j > 0 && (s[j-1] == ' ' || s[j-1] == '"' || s[j-1] == '\'') {
		j--
	}
	k := j
	for k > 0 && (isAlnum(s[k-1]) || s[k-1] == '-' || s[k-1] == '.') {
		k--
	}
	return s[k:j]
}

// numberColumn: the number sits next to another long number
// ("schedstat=( 9487166493 15127253939 2611 )"): a list of counters.
func numberColumn(s string, a, e int) bool {
	isNum := func(t string) bool {
		t = strings.Trim(t, "(),[]{};")
		if len(t) < 6 {
			return false
		}
		for i := 0; i < len(t); i++ {
			if !isDigit(t[i]) {
				return false
			}
		}
		return true
	}
	before := strings.Fields(s[max(0, a-24):a])
	after := strings.Fields(s[e:min(len(s), e+24)])
	if len(before) > 0 && a > 0 && s[a-1] == ' ' && isNum(before[len(before)-1]) {
		return true
	}
	return len(after) > 0 && e < len(s) && s[e] == ' ' && isNum(after[0])
}
