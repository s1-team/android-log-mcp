package redact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Differential tests for #177: the optimized Text must produce exactly the
// bytes of the frozen reference implementation (redact_ref_test.go), with
// the same placeholder numbering and the same hit statistics.

type pair struct {
	n *Redactor
	o *refRedactor
}

func newPair(t testing.TB, o Options) *pair {
	t.Helper()
	n, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRef(o)
	if err != nil {
		t.Fatal(err)
	}
	return &pair{n: n, o: r}
}

// optionSets are the configurations the tests run under.
func optionSets() map[string]Options {
	return map[string]Options{
		"default":   {},
		"home":      {Home: "/home/alice"},
		"keep":      {KeepPrivateIP: true, KeepDomains: []string{"example.com", "Google.com."}, Home: "/Users/Carol Dee"},
		"winhome":   {Home: `C:\Users\Bob Smith`, Extra: []string{`emp-\d{6}`, `(?i)(ticket)-\d+`}},
		"unicode":   {Home: "/home/ünï", Extra: []string{`用户\d+`}},
		"shorthome": {Home: "/h/x"},
		"disabled":  {Disabled: true},
	}
}

func (p *pair) check(t testing.TB, s string) {
	t.Helper()
	got, want := p.n.Text(s), p.o.Text(s)
	if got != want {
		t.Fatalf("Text differs from the reference for %q\n got: %q\nwant: %q", s, got, want)
	}
}

func (p *pair) final(t testing.TB) {
	t.Helper()
	if a, b := p.n.Stats(), p.o.Stats(); !reflect.DeepEqual(a, b) {
		t.Fatalf("hit stats differ: %v vs %v", a, b)
	}
	if a, b := p.n.Unique(), p.o.Unique(); !reflect.DeepEqual(a, b) {
		t.Fatalf("unique counts differ: %v vs %v", a, b)
	}
}

// edgeCases is the hand-written table: every rule family, boundaries, and
// inputs that stress the prefilter (non-ASCII case folding, existing
// placeholders, partial keywords, CR/LF, invalid UTF-8).
var edgeCases = []string{
	"", " ", "\n", "\r\n", "plain message without anything",
	"[REDACTED:email#1]", "x [REDACTED:ipv4#1] y 8.8.8.8", "[REDACTED:", "[REDACTED:ipv6#3", "REDACTED:", "][REDACTED:a#1]]8.8.4.4",
	"8.8.8.8", "8.8.8", "8.8.8.8.8", "1.2.3.4", "v1.2.3.4", "version=1.2.3.4", "255.255.255.255", "0.0.0.0", "256.1.1.1", "01.02.03.04", "1.2.3.45",
	"::", "::1", "a::b", "1:2:3:4:5:6:7:8", "fe80::1", "2001:db8::1", "12:34:56", "12:34:56.789", "aa:bb:cc:dd:ee:ff", "AA:BB:CC:DD:EE:FF:", "aa:bb:cc:dd:ee",
	"mail a@b.co", "a@b.c", "@b.co", "x@y.zz.", "foo@1.2.com", "lib@1.0.so", "a.b@c-d.org,e@f.io",
	"13812345678", "138 1234 5678", "+8613812345678", "+86 138-1234-5678", "+1 415 555 2671", "+123", "+", "1381234567", "x13812345678",
	"11010519491231002X", "11010519491231002x", "110105194912310021", "11010519491231002", "123456789012345678",
	"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111-1", "5500005555555559", "378282246310005", "12345678901234567890",
	"http://example.com", "https://user:pass@example.com:8080/a?b=c", "ftp://1.2.3.4/x", "content://com.android.providers.media", "file:///a/b", "app://com.foo.bar:123",
	"https://EXAMPLE.COM/PATH", "http://", "://", "a://b", "x://y@z.com", "scheme://@host.com",
	"www.example.com", "foo.com", "foo.COM", "Foo.com", "foo.com1", "foo.com-x", "foo.com(", "a.b.c.d.e.f.com", "libc.so", "java.lang.String", "x.com.", "-a.com", "a_b.com",
	"/home/alice/x", "/home/", "/home", "/Users/bob", "C:\\Users\\Bob\\x", "c:/users/bob smith/docs/", `\\Users\\x`, `\Users\y`, "C:\\Users\\", "/home/alice", "/HOME/alice",
	"/Users/Carol Dee/x", "/users/carol dee/x", `c:\users\bob smith\x`, `C:\USERS\BOB SMITH`, "/home/ünï/x", "/HOME/ÜNÏ/x", "/h/x/y",
	"Authorization: Bearer abc123def456", "authorization: abc", "AUTHORIZATION=Basic dXNlcjpwYXNz", "Bearer", "bearer abcdefgh1", "Basic abcdefg1", "basic information",
	"Cookie: a=b", "cookie=a=b; c=d", "Set-Cookie: s=1", "cookies: x",
	"token=abcdef123456", "TOKEN: 'abcdef123456'", `"password": "hunter2"`, "api_key=abc", "apikey = zzz", "Secret=\"x y\"", "credentials: tok_abcdef123", "removeWindowToken: Attempted to remove", "access-key=AKIAABCDEFGHIJKLMNOP",
	"imei=356938035643809", "serial: ABC12345", "ANDROID_ID=0123456789abcdef", "serialno=unknown", "advertising_id=abcdefgh-12", "oaid=1234567890", "gaid:abcdefgh1",
	"ghp_0123456789abcdef0123456789abcdef0123", "github_pat_" + strings.Repeat("a", 50), "npm_" + strings.Repeat("A", 36), "sk-" + strings.Repeat("x", 20), "sk-ant-" + strings.Repeat("y", 24), "AKIAABCDEFGHIJKLMNOP", "ASIAABCDEFGHIJKLMNOP",
	"AIza" + strings.Repeat("z", 35), "xoxb-1234567890-ab", "glpat-" + strings.Repeat("q", 20), "-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "eyJ short",
	"?sig=abcdef", "&code=123456&x", "?KEY=abc", "&nonce=zzzzzz", "a?b=c",
	// non-ASCII case folding: U+017F folds to s, U+212A to k
	"ſecret=abcdefgh1234", "to\u212Aen=abcdefgh1234", "\u212Aey=abcdefgh1234", "pa\u017F\u017Fword=abcdefgh1234", "C:\\U\u017Fers\\bob\\x", "\u017Ferial=ABCD12345678",
	"AUTHORIZATION: Bearer abcdefgh1234", "ÄUTHORIZATION=abcdefgh1", "Cookie: é=1", "用户 13812345678 邮件 a@b.com", "\xff\xfe 8.8.8.8 \xc0\x80", "8.8.8.8\x00x", "tok\x00en=abcdefgh1234",
	"x\r\n8.8.8.8\r\n4111111111111111\n", "line1\nline2 a@b.com\nline3 8.8.4.4 token=abcdefgh1234\n",
	"4111 1111 1111", "4111 1111 1111 1111 123", "4111-1111 1111-1111", "41111 1111 1111 11111", "411 1111 1111 1111", "4111  1111 1111 1111", "x4111 1111 1111 1111x", "1 4111 1111 1111", "5500 0055 5500 0004 99",
	"138 12345678", "1381234 5678", "138-1234-5678", "13 812345678", "1381234567 8", "113812345678", "++8613812345678", "86 13812345678", "12345678901", "1381234 567",
	"::ffff:1.2.3.4", "1:2:3:4:5:6:7", "1:2:3:4:5:6:7:8:9", "a:b", "ab:cd:ef", "dead:beef:cafe:babe:1234:5678:9abc:def0", "FE80::", "::a", "a::", ":::",
	"x/foo.com", "a-.com", ".com", "-foo.com", "com.foo.com", "foo.com.com", "a.b.com", "Foo.com bar.org", "foo_.com", "$foo.com", "#foo.com", "@foo.com", "a@foo.com", `\foo.com`, "xn--abc.com", "1.com", "1.2.com", "java.foo.com", "foo.java.com", "persist.sys.com",
	"imei = 123456789012345", "imei\t:\t123456789012345", "imei \"=123456789012345", "serial_number:ABCD12345", "serialnumber=ABCD12345", "serial_no=ABCD12345", "androidid=0123456789ab", "android id=0123456789ab", "ssaid:0123456789ab", "ADVERTISINGID=abcdefgh1", "oaid\v=abcdefgh1",
	"IntendedVsync=6355123456789012", "schedstat=( 9487166493 15127253939 2611 )", "elapsed 15000000000 ns", "#00 pc 0000000000012345 /system/lib64/libc.so", "0x7f12345678 deadbeefcafebabe 4111111111111111",
}

func TestEquivalenceEdgeCases(t *testing.T) {
	for name, o := range optionSets() {
		t.Run(name, func(t *testing.T) {
			p := newPair(t, o)
			for _, s := range edgeCases {
				p.check(t, s)
			}
			// the same inputs again (placeholders now exist, numbering is stable),
			// all joined, and each one glued to its neighbours
			p.check(t, strings.Join(edgeCases, "\n"))
			p.check(t, strings.Join(edgeCases, " "))
			for i := range edgeCases {
				p.check(t, edgeCases[i])
				if i > 0 {
					p.check(t, edgeCases[i-1]+edgeCases[i])
					p.check(t, edgeCases[i-1]+" "+edgeCases[i])
				}
			}
			p.final(t)
		})
	}
}

// testFixtureStrings collects every string literal of the package's own
// test files (the fixtures of the existing tests) plus the generator's
// snippets.
func testFixtureStrings(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		fs := token.NewFileSet()
		af, err := parser.ParseFile(fs, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil && s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
			return true
		})
	}
	return out
}

func TestEquivalenceExistingFixtures(t *testing.T) {
	strs := testFixtureStrings(t)
	if len(strs) < 300 {
		t.Fatalf("only %d fixture strings found", len(strs))
	}
	for name, o := range optionSets() {
		t.Run(name, func(t *testing.T) {
			p := newPair(t, o)
			for _, s := range strs {
				p.check(t, s)
			}
			p.final(t)
		})
	}
}

func TestEquivalenceRealCaptures(t *testing.T) {
	files, _ := filepath.Glob("../testdata/api29/*")
	if len(files) == 0 {
		t.Skip("no fixtures under ../testdata/api29")
	}
	stride := 10
	if testing.Short() {
		stride = 20
	}
	for _, name := range []string{"default", "home", "keep"} {
		o := optionSets()[name]
		t.Run(name, func(t *testing.T) {
			p := newPair(t, o)
			n := 0
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(string(b), "\n")
				for i := 0; i < len(lines); i += stride {
					p.check(t, lines[i]) // a whole line, header included
					if j := strings.Index(lines[i], ": "); j >= 0 {
						p.check(t, lines[i][j+2:]) // the message, as the report passes it
					}
					n++
				}
				if len(b) < 70000 { // bulk text: one call on a whole file
					p.check(t, string(b))
				}
			}
			if n < 100 {
				t.Fatalf("only %d lines compared", n)
			}
			p.final(t)
		})
	}
}

var fuzzAtoms = []string{
	"0", "1", "4", "9", "13", "138", "1234", "4111", "11010519491231002", "8.8.8.8", "10.0.0.1", ".", "..", ":", "::", "@", "+", "-", "_", "/", "\\", "=", "?", "&", " ", "  ", "\t", "\n", "\r", "[", "]", "#", "\"", "'", ",", ";", "(", ")",
	"a", "b", "e", "f", "x", "A", "F", "com", ".com", "co.uk", "example", "example.com", "www.", "http://", "https://", "ftp://", "://", "user:pw@", "@host.org", "token", "Token", "TOKEN", "key", "api_key", "secret", "passw", "password", "credential",
	"Authorization", "authorization", "Bearer ", "Basic ", "Cookie", "cookie", "Set-Cookie", "serial", "imei", "android_id", "Users", "users", "/home/", "/Users/", "C:\\Users\\", "/home/alice", "alice", "eyJ", "eyJhbGciOiJIUzI1NiJ9", "ghp_", "gh", "github_pat_", "sk-", "npm_",
	"AKIA", "ASIA", "AIza", "xox", "xoxb-", "glpat-", "-----BEGIN ", "PRIVATE KEY-----", "sig", "code", "sid", "[REDACTED:", "REDACTED:", "#1]", "ipv4#1]", "ſ", "K", "é", "用", "\xff", "\x00",
	"aa:bb:cc:dd:ee:ff", "fe80::1", "+86", "4111 ", "-1111", "1111 ", "138", "1234 ", "imei", "serial=", "android_id", " : ", "::", "foo.", ".org", "bar.com", "mNumber=", "elapsed", " ns", "ms", "pc ", "0x", "deadbeef", "version", "v", "Chrome/", "so", "id",
}

func randText(rng *rand.Rand) string {
	var b strings.Builder
	for n := 1 + rng.Intn(40); n > 0; n-- {
		switch rng.Intn(6) {
		case 0:
			b.WriteString(genSnippets[rng.Intn(len(genSnippets))])
		case 1:
			b.WriteString(edgeCases[rng.Intn(len(edgeCases))])
		case 2:
			b.WriteString(genWords[rng.Intn(len(genWords))])
		default:
			b.WriteString(fuzzAtoms[rng.Intn(len(fuzzAtoms))])
		}
		if rng.Intn(4) == 0 {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func TestEquivalenceRandomized(t *testing.T) {
	iters := 1500
	if testing.Short() {
		iters = 800
	}
	if v, err := strconv.Atoi(os.Getenv("REDACT_EQ_ITERS")); err == nil && v > 0 {
		iters = v // soak run: REDACT_EQ_ITERS=100000 go test -run Randomized
	}
	for name, o := range optionSets() {
		t.Run(name, func(t *testing.T) {
			p := newPair(t, o)
			rng := rand.New(rand.NewSource(177))
			for i := 0; i < iters; i++ {
				p.check(t, randText(rng))
			}
			// generated log messages at several sensitivity levels, per line and in bulk
			for si, sens := range []float64{0, 0.01, 0.1, 0.5} {
				lines := synthLines(60, int64(100+si), sens)
				for _, l := range lines {
					p.check(t, l)
				}
				p.check(t, strings.Join(lines, "\n"))
			}
			p.final(t)
		})
	}
}

// The same generator on several seeds so a regression is reproducible from
// the seed in the failure message.
func TestEquivalenceSeeds(t *testing.T) {
	p := newPair(t, Options{Home: "/home/alice"})
	seeds := 15
	if testing.Short() {
		seeds = 5
	}
	for seed := int64(0); seed < int64(seeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		for i := 0; i < 100; i++ {
			s := randText(rng)
			got, want := p.n.Text(s), p.o.Text(s)
			if got != want {
				t.Fatalf("seed %d #%d: %q\n got: %q\nwant: %q", seed, i, s, got, want)
			}
		}
	}
}

var (
	fuzzMu    sync.Mutex
	fuzzPairs = map[uint8]*pair{}
)

// FuzzRedactEquivalence: any input, under one of several option sets, must
// redact to exactly the bytes the reference produces. The pairs persist
// across inputs (the numbering of placeholders stays in lock step).
func FuzzRedactEquivalence(f *testing.F) {
	for i, s := range edgeCases {
		f.Add(s, uint8(i))
	}
	for i, s := range genSnippets {
		f.Add(s, uint8(i))
	}
	f.Add("user=/home/alice/x ip 8.8.8.8 a@b.com token=abcdefgh1234", uint8(1))
	f.Fuzz(func(t *testing.T, s string, mode uint8) {
		names := []string{"default", "home", "keep", "winhome", "unicode", "shorthome"}
		name := names[int(mode)%len(names)]
		fuzzMu.Lock()
		defer fuzzMu.Unlock()
		p := fuzzPairs[mode%uint8(len(names))]
		if p == nil {
			p = newPair(t, optionSets()[name])
			fuzzPairs[mode%uint8(len(names))] = p
		}
		p.check(t, s)
	})
}
