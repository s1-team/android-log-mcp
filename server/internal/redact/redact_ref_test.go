package redact

// Frozen reference copy of the pre-#177 implementation of Text (the
// sequential "run every rule's regexp over the whole string" version). The
// differential tests assert that the optimized Text produces byte-identical
// output with an identical placeholder numbering. Do not edit these rules
// or "improve" this file: a behaviour change must be made in redact.go and
// then deliberately mirrored here, with the reason in the commit message.

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

type refRule struct {
	typ   string
	re    *regexp.Regexp
	group int
	need  string
	ok    func(s string, start, end int, v string) bool
}

// refRedactor shares the key, id tables and placeholder() of a real
// Redactor but applies the reference rules.
type refRedactor struct {
	*Redactor
	rules []refRule
}

func newRef(o Options) (*refRedactor, error) {
	base, err := New(o)
	if err != nil {
		return nil, err
	}
	r := &refRedactor{Redactor: base}
	for i, x := range o.Extra {
		re, err := regexp.Compile(x)
		if err != nil {
			return nil, fmt.Errorf("--redact-extra #%d %q: %v", i+1, x, err)
		}
		g := 0
		if re.NumSubexp() > 0 {
			g = 1
		}
		r.rules = append(r.rules, refRule{typ: "custom", re: re, group: g})
	}
	if hr := refHomeRule(o.Home); hr != nil {
		r.rules = append(r.rules, *hr)
	}
	r.rules = append(r.rules, refBuiltin(o)...)
	return r, nil
}

// Text is the old Redactor.Text.
func (r *refRedactor) Text(s string) string {
	if r.opt.Disabled || s == "" {
		return s
	}
	for i := range r.rules {
		s = r.apply(&r.rules[i], s)
	}
	return s
}

var _ = net.ParseIP

// homeRule redacts the literal home directory. The directory separators match
// either slash type, repeated (a JSON-escaped path has "\\\\").
func refHomeRule(home string) *refRule {
	h := strings.TrimRight(home, `/\`)
	if len(h) < 3 {
		return nil
	}
	parts := regexp.MustCompile(`[\\/]+`).Split(h, -1)
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
	return &refRule{typ: "home_dir", re: re, group: 1}
}

func (r *refRedactor) apply(ru *refRule, s string) string {
	if ru.need != "" && !strings.ContainsAny(s, ru.need) {
		return s
	}
	ms := ru.re.FindAllStringSubmatchIndex(s, -1)
	if len(ms) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		a, e := m[2*ru.group], m[2*ru.group+1]
		if a < 0 || a < last {
			continue
		}
		v := s[a:e]
		if v == "" || strings.Contains(v, prefix) || refInsidePlaceholder(s, a) {
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
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// insidePlaceholder reports whether position i lies inside "[REDACTED:...]".
func refInsidePlaceholder(s string, i int) bool {
	j := strings.LastIndex(s[:i], prefix)
	if j < 0 {
		return false
	}
	k := strings.IndexByte(s[j:], ']')
	return k < 0 || j+k >= i
}

const refUserName = `[^\\/\s"'<>|:*?\[\]]+`

func refBuiltin(o Options) []refRule {
	keepIP := o.KeepPrivateIP
	return []refRule{
		// user names inside home paths: /home/<user>, /Users/<user>,
		// C:\Users\<user> (either slash type, JSON-escaped backslashes, any
		// case for the Windows form; a Windows name may contain spaces)
		{typ: "username", need: "uU", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])[A-Za-z]:[\\/]+Users[\\/]+(` + refUserName + `(?: ` + refUserName + `)+)[\\/]`), group: 1},
		{typ: "username", need: "uU", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:[A-Za-z]:[\\/]+|\\+)Users[\\/]+(` + refUserName + `)`), group: 1},
		{typ: "username", need: "/", re: regexp.MustCompile(`/(?:home|Users)/(` + refUserName + `)`), group: 1},
		// credentials embedded in URLs: scheme://user:password@host
		{typ: "password", need: "@", re: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`), group: 1},
		// Authorization headers (value, with or without scheme)
		{typ: "token", need: "aA", re: regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|digest|token|negotiate)\s+)?([A-Za-z0-9._~+/=-]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return tokenLike(v) }},
		{typ: "token", need: "bB", re: regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return tokenLike(v) }},
		{typ: "jwt", need: "e", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)},
		// Cookie / Set-Cookie header values (whole value: several name=value pairs)
		{typ: "cookie", need: "cC", re: regexp.MustCompile(`(?i)\b(?:set-)?cookie["']?\s*[:=]\s*["']?([^\s"'][^\n"']*)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return strings.Contains(v, "=") && credentialValue(v) }},
		// well-known key formats
		{typ: "secret", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{50,}|npm_[A-Za-z0-9]{36}|sk-(?:proj-|ant-|live-|test-)?[A-Za-z0-9_-]{20,}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|xox[abposr]-[A-Za-z0-9-]{10,}|glpat-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)},
		// key=value / "key": "value" with a sensitive key name (also URL query params)
		{typ: "credential", need: ":=", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])["']?[A-Za-z0-9_.-]*?(?:api[_-]?key|apikey|secret|token|passw(?:or)?d|access[_-]?key|private[_-]?key|auth[_-]?key|credentials?)s?["']?\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s,;&"'}\])]+)`), group: 1,
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
		{typ: "device_id", re: regexp.MustCompile(`(?i)\b(?:imei|imsi|iccid|meid|esn|serial(?:no|number|_number)?|android_?id|ssaid|advertising_?id|gaid|oaid)["']?\s*[:=]\s*["']?([0-9A-Za-z-]{8,40})`), group: 1,
			ok: func(s string, a, e int, v string) bool { return credentialValue(v) && !strings.EqualFold(v, "unknown") }},
		// other sensitive URL query parameters
		{typ: "url_param", need: "?&", re: regexp.MustCompile(`(?i)[?&](?:sig|signature|code|key|auth|ticket|sid|session|otp|nonce|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&#\s"'<>]+)`), group: 1,
			ok: func(s string, a, e int, v string) bool { return credentialValue(v) }},
		{typ: "email", need: "@", re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,24}\b`),
			ok: func(s string, a, e int, v string) bool {
				if a > 0 && (isAlnum(s[a-1]) || s[a-1] == '.' || s[a-1] == '/' || s[a-1] == '@') {
					return false // part of a word or a file path
				}
				// versioned HIDL/AIDL names: android.hardware.sensors@1.0.so, foo@2.1-impl
				dom := v[strings.IndexByte(v, '@')+1:]
				first, _, _ := strings.Cut(dom, ".")
				return strings.Trim(first, "0123456789") != ""
			}},
		{typ: "id_card", re: regexp.MustCompile(`\d{17}[\dXx]`),
			ok: func(s string, a, e int, v string) bool {
				return numBoundary(s, a, e) && validCNID(v) && !reNumericUnit.MatchString(strings.ToLower(s[e:min(len(s), e+8)]))
			}},
		{typ: "bank_card", re: regexp.MustCompile(`\d{4}(?:[ -]\d{4}){2,3}(?:[ -]\d{1,3})?|\d{13,19}`),
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
		{typ: "phone", re: regexp.MustCompile(`(?:\+?86[ -]?)?1[3-9]\d(?:[ -]?\d{4}){2}`),
			ok: func(s string, a, e int, v string) bool {
				return numBoundary(s, a, e) && !(a > 0 && s[a-1] == '+') && !numericContext(s, a, e) && !numberColumn(s, a, e)
			}},
		{typ: "phone", need: "+", re: regexp.MustCompile(`\+[1-9]\d{0,2}[ -]?(?:\(\d{1,4}\)[ -]?)?\d{2,4}(?:[ -]?\d{2,4}){1,3}`),
			ok: func(s string, a, e int, v string) bool {
				n := len(digitsOnly(v))
				if len(v) == n+1 && n < 10 { // compact "+NNNN…" needs E.164 length; "+28800000" is an offset
					return false
				}
				return numBoundary(s, a, e) && n >= 8 && n <= 15 && !numericContext(s, a, e)
			}},
		{typ: "mac", need: ":", re: regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`),
			ok: func(s string, a, e int, v string) bool {
				if e < len(s) && s[e] == ':' {
					return false
				}
				l := strings.ToLower(v)
				return l != "00:00:00:00:00:00" && l != "02:00:00:00:00:00" && l != "ff:ff:ff:ff:ff:ff"
			}},
		{typ: "ipv4", need: ".", re: regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`),
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
		{typ: "ipv6", need: ":", re: regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}`),
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
		{typ: "domain", need: ":", re: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://(?:[^\s/@?]*@)?([^\s/:?#\[\]@"'<>\\]+)`), group: 2,
			ok: func(s string, a, e int, v string) bool {
				m := reScheme.FindStringSubmatch(s[:a])
				if m == nil || nonNetSchemes[strings.ToLower(m[1])] {
					return false
				}
				return urlHost(v) && !packageAuthority(v) && !keptDomain(o.KeepDomains, v)
			}},
		// 2) bare hostnames in text: lower-case labels ending in a common public TLD,
		//    not shaped like a reverse-DNS Java/Android name
		{typ: "domain", need: ".", re: regexp.MustCompile(`(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,12}`),
			ok: func(s string, a, e int, v string) bool { return bareHost(s, a, e, v) && !keptDomain(o.KeepDomains, v) }},
	}
}
