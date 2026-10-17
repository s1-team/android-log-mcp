package redact

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// Synthetic logcat message generator shared by the benchmarks and the
// differential tests. Deterministic for a given seed.

var genWords = strings.Fields(`the a of to and in is for on with socket connection window focus
activity manager started stopped resumed paused binder transaction failed timeout retry cache hit miss
frame dropped vsync buffer queue surface layer display refresh rate power wake lock acquired released
network state changed wifi connected scan results update job scheduled alarm fired broadcast delivered
permission denied granted service bound unbound token expired renewed session opened closed bytes read written`)

var genTags = []string{"ActivityManager", "WindowManager", "chatty", "NetworkMonitor", "wpa_supplicant", "libc", "art", "InputDispatcher"}

// sensitive/near-sensitive fragments covering every rule family, including
// negatives the validators must reject.
var genSnippets = []string{
	"user=/home/alice/.config/app", "path C:\\Users\\Bob Smith\\Documents\\x.txt", "/Users/carol/Library/Caches",
	"https://bob:hunter2pw@example.com/path?sig=abcdef123456&x=1", "Authorization: Bearer abcDEF123456ghiJKL789",
	"authorization=Basic dXNlcjpwYXNzd29yZA==", "Cookie: sid=abc123; theme=dark", "Set-Cookie: SESSION=deadbeefcafe; Path=/",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
	"ghp_0123456789abcdef0123456789abcdef0123", "sk-proj-abcdefghijklmnopqrstuvwx", "AKIAABCDEFGHIJKLMNOP", "xoxb-1234567890-abcdef",
	"-----BEGIN RSA PRIVATE KEY-----", `"api_key": "s3cr3tvalue99"`, "password=hunter2hunter2", "access-token: abc.def.ghi123",
	"removeWindowToken: Attempted to remove non-existing token: android.os.Binder@1a2b3c", "imei=356938035643809", "serial=ABCD1234EFGH",
	"android_id: 9774d56d682e549c", "serialno=unknown", "GET /v1/x?code=123456&other=1", "mail alice@example.com sent", "android.hardware.sensors@1.0.so",
	"id 11010519491231002X ok", "id 110105194912310021", "card 4111 1111 1111 1111", "4111111111111111", "IntendedVsync=6355123456789012",
	"mNumber=13812345678", "tel +86 138 1234 5678", "+1 (415) 555-2671", "+28800000", "elapsed 15000000000 ns", "schedstat=( 9487166493 15127253939 2611 )",
	"mac aa:bb:cc:dd:ee:ff", "mac 00:00:00:00:00:00", "ip 8.8.8.8", "ip 192.168.1.20", "ip 127.0.0.1", "version 1.2.3.4", "Chrome/83.0.41.10", "10.0.0.1:8080",
	"fe80::1ff:fe23:4567:890a", "2001:db8::ff00:42:8329", "time 12:34:56.789", "http://www.google.com/gen_204", "app://com.google.android.apps.nexuslauncher:10108",
	"content://com.android.providers.media/external", "file:///sdcard/x", "connect to time.android.com failed", "libc.so loaded", "persist.sys.user.id=3",
	"com.example.app.MainActivity", "android.app.ActivityThread.main(ActivityThread.java:7356)", "#00 pc 0000000000012345 /system/lib64/libc.so",
	"deadbeef12345678 cafebabe87654321", "[REDACTED:email#1] stays", "x[REDACTED:ipv4#2]y 8.8.4.4", "ſecret=abcdefgh12 ſ", "Kelvin K token=abcdefgh1234", "用户 alice@例子.com 手机 13812345678",
	"foo@bar.baz.qux", "v1.2.3.4", "key=value&sig=abcdef1234", "?sig=abc&auth=xyz123456", "a::b", "::1", "1:2:3:4:5:6:7:8", "/home/", "/home/x", "C:\\Users\\", `\\Users\\dave`,
}

func genLine(rng *rand.Rand, minWords, maxWords int, sensitive float64) string {
	n := minWords + rng.Intn(maxWords-minWords+1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch rng.Intn(14) {
		case 0:
			fmt.Fprintf(&b, "%d", rng.Intn(100000))
		case 1:
			fmt.Fprintf(&b, "0x%x", rng.Uint32())
		case 2:
			fmt.Fprintf(&b, "%s.%s.%s", genWords[rng.Intn(len(genWords))], genWords[rng.Intn(len(genWords))], genTags[rng.Intn(len(genTags))])
		case 3:
			fmt.Fprintf(&b, "%s=%d", genWords[rng.Intn(len(genWords))], rng.Intn(5000))
		case 4:
			fmt.Fprintf(&b, "%s:", genTags[rng.Intn(len(genTags))])
		default:
			b.WriteString(genWords[rng.Intn(len(genWords))])
		}
		if rng.Float64() < sensitive {
			b.WriteByte(' ')
			b.WriteString(genSnippets[rng.Intn(len(genSnippets))])
		}
	}
	return b.String()
}

// synthLines returns n messages: short (3-8 words), medium (10-25) and long
// (40-90), with `sensitive` the per-word probability of a fragment.
func synthLines(n int, seed int64, sensitive float64) []string {
	rng := rand.New(rand.NewSource(seed))
	out := make([]string, n)
	for i := range out {
		switch rng.Intn(10) {
		case 0, 1, 2, 3:
			out[i] = genLine(rng, 3, 8, sensitive)
		case 4, 5, 6, 7, 8:
			out[i] = genLine(rng, 10, 25, sensitive)
		default:
			out[i] = genLine(rng, 40, 90, sensitive)
		}
	}
	return out
}

// realMessages returns the message part of up to max lines of the real
// API 29 captures (the same slicing the report uses: after the first ": ").
func realMessages(max int) []string {
	files, _ := filepath.Glob("../testdata/api29/*.logcat.txt")
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.Index(ln, ": "); i >= 0 && len(out) < max {
				out = append(out, ln[i+2:])
			}
		}
	}
	return out
}
