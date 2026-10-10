package redact

import (
	"regexp"
	"strings"
	"testing"
)

func newT(t *testing.T, o Options) *Redactor {
	t.Helper()
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// redacted asserts that in → output contains no secret and has a placeholder of typ.
func positive(t *testing.T, r *Redactor, typ, in string, secrets ...string) string {
	t.Helper()
	out := r.Text(in)
	if !strings.Contains(out, "[REDACTED:"+typ+"#") {
		t.Errorf("%s: no %s placeholder in %q → %q", typ, typ, in, out)
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%s: secret %q survived: %q", typ, s, out)
		}
	}
	return out
}

func negative(t *testing.T, r *Redactor, in string) {
	t.Helper()
	if out := r.Text(in); out != in {
		t.Errorf("false positive: %q → %q", in, out)
	}
}

func TestBearerAndAuthorization(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "token", "Authorization: Bearer abcDEF123456ghiJKL789", "abcDEF123456ghiJKL789")
	positive(t, r, "token", `--> GET /v1 header authorization=Basic dXNlcjpwYXNzd29yZA==`, "dXNlcjpwYXNzd29yZA==")
	positive(t, r, "token", "auth header: Bearer 7f3a9c0d2e1b4a5f6e7d8c9b", "7f3a9c0d2e1b4a5f6e7d8c9b")
	negative(t, r, "Basic information about the device")
	negative(t, r, "Bearer token missing")
	negative(t, r, "Authorization failed for uid 10146")
}

func TestJWT(t *testing.T) {
	r := newT(t, Options{})
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	out := positive(t, r, "jwt", "id_token received "+jwt, jwt, "eyJzdWIi")
	if !strings.HasPrefix(out, "id_token received ") {
		t.Errorf("context damaged: %q", out)
	}
	positive(t, r, "token", "Authorization: Bearer "+jwt, "SflKxwRJ")
	negative(t, r, "eyJ is a common prefix")
}

func TestSecretPrefixes(t *testing.T) {
	r := newT(t, Options{})
	for _, s := range []string{
		"gh" + "p_1234567890abcdefghijABCDEFGHIJ123456",
		"gh" + "o_16C7e42F292c6912E7710c838347Ae178B4a",
		"gi" + "thub_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMN",
		"np" + "m_abcdefghijklmnopqrstuvwxyz0123456789",
		"sk" + "-proj-abcdefghijklmnopqrstuvwxyz012345",
		"AK" + "IAIOSFODNN7EXAMPLE",
		"AI" + "zaSyA-1234567890abcdefghijklmnopqrstu",
		"xo" + "xb-123456789012-1234567890123-abcdefghijklmnop",
		"gl" + "pat-abcdefghij1234567890",
	} {
		positive(t, r, "secret", "loaded key "+s+" ok", s)
	}
	negative(t, r, "sk-1 is fine")                   // too short
	negative(t, r, "npm_config_cache=/tmp/x")        // npm env var name
	negative(t, r, "AKIA is an AWS prefix")          // no key body
	negative(t, r, "task-12345 desk-abcdef started") // sk- inside a word
	negative(t, r, "ghp_ prefix alone")
}

func TestKeyValueCredentials(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "credential", "login password=hunter2secret user=bob", "hunter2secret")
	positive(t, r, "credential", `{"api_key":"AbC123xyz789","n":1}`, "AbC123xyz789")
	positive(t, r, "credential", "client_secret: s3cr3t-value", "s3cr3t-value")
	positive(t, r, "credential", "passwd='p@ss w0rd'", "p@ss w0rd")
	positive(t, r, "credential", "X-Api-Key: 0123abcd4567ef89", "0123abcd4567ef89")
	out := positive(t, r, "credential", `{"access_token": "tok_9f8e7d6c5b4a", "expires_in": 3600}`, "tok_9f8e7d6c5b4a")
	if !strings.Contains(out, `"expires_in": 3600`) {
		t.Errorf("neighbour damaged: %s", out)
	}
	// not secrets: window tokens / object refs / null / booleans / small counts / names only
	negative(t, r, "addWindow token=android.os.BinderProxy@6c2b1a8 type=2")
	negative(t, r, "AppWindowToken{7d2e1f3 token=Token{a1b2c3 ActivityRecord{ff00 u0 com.example/.Main t12}}}")
	negative(t, r, "token=null password=")
	negative(t, r, "hasPassword=false secureToken=true")
	negative(t, r, "max_tokens=512")
	negative(t, r, "TokenManager: refreshing token for account")
}

func TestURLParams(t *testing.T) {
	r := newT(t, Options{})
	out := positive(t, r, "credential", "GET https://api.example.com/v1/feed?page=2&token=abc123def456&lang=en", "abc123def456")
	if !strings.Contains(out, "page=2") || !strings.Contains(out, "&lang=en") || !strings.Contains(out, "https://[REDACTED:domain#") {
		t.Errorf("url structure damaged: %s", out)
	}
	positive(t, r, "url_param", "https://cdn.example.com/a.png?Expires=1791572202&Signature=Zm9vYmFyYmF6&Key-Pair-Id=K2AB", "Zm9vYmFyYmF6")
	positive(t, r, "url_param", "redirect https://example.com/cb?code=4/0AY0e-g7abc&state=xyz", "4/0AY0e-g7abc")
	positive(t, r, "password", "connect https://bob:pa55w0rd@db.example.com:5432/x", "pa55w0rd")
	if out := r.Text("GET https://api.example.com/v1/feed?page=2&lang=en"); !regexp.MustCompile(`^GET https://\[REDACTED:domain#\d+\]/v1/feed\?page=2&lang=en$`).MatchString(out) {
		t.Errorf("only the host should change: %q", out)
	}
	negative(t, r, "loaded file:///data/app/com.example-1/base.apk")
}

func TestEmail(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "email", "user john.doe+tag@example.co.uk logged in", "john.doe+tag@example.co.uk")
	negative(t, r, "java.lang.Object@1a2b3c4d")
	negative(t, r, "com.example.Foo$Bar@7d2e1f3 resumed")
	negative(t, r, "package androidx.core@1.12.0")
}

func TestPhone(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "phone", "sms to 13812345678 sent", "13812345678")
	positive(t, r, "phone", "call +86 138-1234-5678", "138-1234-5678")
	positive(t, r, "phone", "contact +1 (415) 555-2671", "555-2671")
	negative(t, r, "1791572202 1791572202.589379 epoch")          // epoch seconds (10 digits)
	negative(t, r, "t=1791572202589 ms")                          // epoch millis (13 digits)
	negative(t, r, "elapsed 17915722025ns")                       // duration with unit glued
	negative(t, r, "pid 13812 tid 13813 uid 10146")               // pids
	negative(t, r, "tz +0800 offset")                             // tz offset
	negative(t, r, "build 10/QSR1.211112.011/13135432:userdebug") // fingerprint numbers
	negative(t, r, "version 1.3812345678")                        // part of decimal
}

func TestCNIDCard(t *testing.T) {
	r := newT(t, Options{})
	// synthetic IDs with valid checksums (GB 11643)
	ok := []string{"11010519491231002X", "440308199901011234"}
	for _, id := range ok {
		if !validCNID(id) {
			t.Fatalf("test vector %s should be valid", id)
		}
		positive(t, r, "id_card", "id "+id+" verified", id)
	}
	negative(t, r, "id 440308199901011238 bad checksum")
	negative(t, r, "ns 179157220258937900 timestamp") // 18 digits, no valid date
	negative(t, r, "seq 123456789012345678")
}

func TestBankCard(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "bank_card", "card 4111111111111111 charged", "4111111111111111")
	positive(t, r, "bank_card", "card 4111 1111 1111 1111 charged", "4111 1111 1111 1111")
	positive(t, r, "bank_card", "unionpay 6222021234567890128", "6222021234567890128")
	negative(t, r, "card 4111111111111112 invalid luhn")
	negative(t, r, "ts 1791572202589379 us")                 // epoch µs, Luhn-irrelevant (prefix 1)
	negative(t, r, "#01 pc 0000000000097146  /apex/libc.so") // stack frame offset
	negative(t, r, "r8  0000000010090008  r9  0000000000000000")
	negative(t, r, "ptr 0x4111111111111111") // hex literal
	negative(t, r, "a8b4111111111111111c")   // inside a hex blob
}

func TestIP(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "ipv4", "connect to 203.0.113.42:443", "203.0.113.42")
	positive(t, r, "ipv4", "dns 10.0.2.3", "10.0.2.3")
	positive(t, r, "ipv6", "addr 2001:db8:85a3::8a2e:370:7334 up", "2001:db8:85a3::8a2e:370:7334")
	negative(t, r, "127.0.0.1 and 0.0.0.0")
	negative(t, r, "version 1.2.3 build 10.0.2")
	negative(t, r, "at 03:02:39.901 and 12:34:56")
	negative(t, r, "chrome 120.0.6099.230 android 14.0.0.1.2") // 4-part versions after a name are not IPs... see rule
	keep := newT(t, Options{KeepPrivateIP: true})
	negative(t, keep, "dns 10.0.2.3 gw 192.168.1.1 fe80::1")
	positive(t, keep, "ipv4", "public 8.8.8.8", "8.8.8.8")
}

func TestMAC(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "mac", "wlan0 hwaddr 3c:5a:b4:01:02:03", "3c:5a:b4:01:02:03")
	negative(t, r, "time 03:02:39")
}

func TestStablePlaceholders(t *testing.T) {
	r := newT(t, Options{})
	a := r.Text("mail a@example.com and b@example.com then a@example.com")
	if a != "mail [REDACTED:email#1] and [REDACTED:email#2] then [REDACTED:email#1]" {
		t.Fatalf("got %q", a)
	}
	if got := r.Text("again b@example.com"); got != "again [REDACTED:email#2]" {
		t.Fatalf("not stable across calls: %q", got)
	}
	// a different report key → same numbering scheme, but the key is random and not exposed
	r2 := newT(t, Options{})
	if r2.Text("x@example.com") != "[REDACTED:email#1]" {
		t.Fatal("numbering must start at 1 per report")
	}
	st := r.Stats()
	if st["email"] != 4 || r.Unique()["email"] != 2 {
		t.Fatalf("stats %v unique %v", st, r.Unique())
	}
	// re-redacting output is idempotent
	if r.Text(a) != a {
		t.Fatal("placeholders must not be re-redacted")
	}
}

func TestExtraAndDisabled(t *testing.T) {
	r := newT(t, Options{Extra: []string{`emp-\d{6}`, `user=(\w+)`}})
	positive(t, r, "custom", "badge emp-123456 ok", "emp-123456")
	out := positive(t, r, "custom", "user=alice logged", "alice")
	if !strings.HasPrefix(out, "user=") {
		t.Errorf("group-only redaction expected: %q", out)
	}
	if _, err := New(Options{Extra: []string{"("}}); err == nil {
		t.Fatal("bad regex must error")
	}
	off := newT(t, Options{Disabled: true})
	negative(t, off, "a@example.com 13812345678 4111111111111111")
}

// Structural tokens of real logs must survive untouched.
func TestStructureUntouched(t *testing.T) {
	r := newT(t, Options{})
	for _, s := range []string{
		"1791572202.589379 10146  6713  6713 V LogTest : onCreate verbose",
		"at com.example.logtest.Triggers.run(Triggers.java:22)",
		"#00 pc 00000000000943f8  /apex/com.android.runtime/lib64/bionic/libc.so (syscall+24) (BuildId: 1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d)",
		"Build fingerprint: 'google/sdk_gphone_x86_64/generic_x86_64:10/QSR1.211112.011/13135432:userdebug/dev-keys'",
		"pid: 7736, tid: 7736, name: example.logtest  >>> com.example.logtest <<<",
		"signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0",
		"rax 0000000000000000  rbx 00007ef862cd4000  rcx 81c28a362e54aee9  rdx 0000000000000004",
		"am_proc_start: [0,7182,10146,com.example.logtest,activity,{com.example.logtest/com.example.logtest.MainActivity}]",
		"Displayed com.example.logtest/.MainActivity: +18s692ms",
		"Start proc 4668:com.example.logtest/u0a146 for broadcast",
		"CPU usage from 128ms to -17580ms ago (2026-10-10 03:05:52.878 to 2026-10-10 03:06:10.586):",
		"versionCode=1 minSdk=21 targetSdk=29 versionName=1.0.3",
		"GC freed 2019(159KB) AllocSpace objects, 55% free, 1207KB/2743KB, paused 2.126ms total 533.472ms",
	} {
		negative(t, r, s)
	}
}

func TestLuhnAndCNIDHelpers(t *testing.T) {
	if !luhn("4111111111111111") || luhn("4111111111111112") {
		t.Fatal("luhn")
	}
	if validCNID("110105194912310021") == validCNID("11010519491231002X") {
		t.Fatal("cnid checksum must distinguish")
	}
	_ = regexp.MustCompile
}
