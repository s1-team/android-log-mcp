package store

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Issue #22 D1: DecodeRecord must equal json.Unmarshal on every input, in
// the full decoded struct and in whether it fails.

func checkDecode(t testing.TB, line []byte) (fast bool) {
	t.Helper()
	var want, got model.Record
	werr := json.Unmarshal(line, &want)
	gerr := DecodeRecord(line, &got)
	if (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
		t.Fatalf("%q: error %v, encoding/json %v", line, gerr, werr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q:\n got %#v\nwant %#v", line, got, want)
	}
	var probe model.Record
	return decodeFast(line, &probe)
}

// edge cases: each must decode like encoding/json (fast path or fallback)
var decodeSeeds = []string{
	`{"s":1,"b":"0a1b2c3d","t":1791712386578660000,"u":10146,"p":100,"i":101,"l":73,"g":"Tag","m":"hello"}`,
	`{"s":2,"b":"b","t":-5,"u":-1,"p":0,"i":0,"l":69,"g":"T","m":"x","f":"main","n":"com.a:remote","k":"com.a"}`,
	`{}`, `{"s":1}`, `null`, `[]`, `{"s":1}` + "\n", `{"s":1}` + "\r\n", ` {"s":1}`, `{"s":1} x`,
	`{"s":1,"s":2}`, `{"S":3}`, `{"x":1,"s":2}`, `{"s":1,"zz":{"a":[1,2,{"b":null}]}}`, `{"\u0073":4}`,
	`{"s":null}`, `{"b":null}`, `{"s":1.0}`, `{"s":1e3}`, `{"s":-1}`, `{"s":01}`, `{"s":18446744073709551615}`,
	`{"s":18446744073709551616}`, `{"t":9223372036854775807}`, `{"t":-9223372036854775808}`, `{"t":9223372036854775808}`,
	`{"l":255}`, `{"l":256}`, `{"l":-0}`, `{"t":-0}`, `{"t":-}`, `{"s":18446744073709551614}`, `{"s":99999999999999999999}`, `{"t":-9223372036854775809}`, `{"t":1791572737970524000}`, `{"s":1,}`, `{"s" :1}`, `{"s": 1}`, `{"s":1 }`,
	`{"m":"a\"b\\c\/d\b\f\n\r\t"}`, `{"m":"\u003c\u003e\u0026\u2028\u2029"}`, `{"m":"\u00e9\u4e2d"}`,
	`{"m":"\ud83d\ude00"}`, `{"m":"\ud83d"}`, `{"m":"\ude00"}`, `{"m":"\ud83dx"}`, `{"m":"\ud83d\u0041"}`,
	`{"m":"\ud83d\ud83d\ude00"}`, `{"m":"\ude00\ud83d"}`, `{"m":"\uDBFF\uDFFF"}`, `{"m":"\u12"}`, `{"m":"\uzzzz"}`,
	`{"m":"\x"}`, `{"m":"a` + "\x01" + `b"}`, `{"m":"a` + "\x7f" + `b"}`, `{"m":"` + "\xff\xfe" + `"}`,
	`{"m":"` + "\xe4\xb8" + `"}`, `{"m":"` + "\xed\xa0\x80" + `"}`, `{"m":"` + "\xef\xbf\xbd" + `"}`,
	`{"m":"` + "\xc0\xaf" + `"}`, `{"m":"` + "\xf4\x90\x80\x80" + `"}`, `{"m":"unterminated`, `{"m":"x"`, `{"m":1}`,
	`{"g":"a","m":"b\\"}`, `{"m":"\\u0041"}`, `{"m":""}`, `{"m":"` + strings.Repeat("é", 100) + `"}`,
}

func TestDecodeRecordEdgeCases(t *testing.T) {
	for _, s := range decodeSeeds {
		checkDecode(t, []byte(s))
	}
}

// randString mixes ASCII, characters json.Marshal escapes, multi-byte
// runes, invalid UTF-8 and control characters.
func randString(rng *rand.Rand) string {
	alpha := []string{"a", "Z", "0", " ", "\"", "\\", "/", "<", ">", "&", "\n", "\t", "\x00", "\x1f", "\x7f",
		"é", "中", "😀", "\u2028", "\ufffd", "\xff", "\xe4\xb8", "\xed\xa0\x80"}
	n := rng.Intn(40)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(alpha[rng.Intn(len(alpha))])
	}
	return b.String()
}

// Every record json.Marshal writes (as the store does) takes the fast
// path and decodes equal to encoding/json.
func TestDecodeRecordMarshalled(t *testing.T) {
	rng := rand.New(rand.NewSource(22))
	for n := 0; n < decodeIters; n++ {
		r := model.Record{Seq: rng.Uint64() >> uint(rng.Intn(64)), Boot: randString(rng), TimeNs: rng.Int63() - rng.Int63() - int64(rng.Intn(2)),
			UID: rng.Intn(1 << 20), PID: rng.Intn(1<<16) - 10, TID: rng.Intn(1 << 16), Level: model.Level(rng.Intn(256)),
			Tag: randString(rng), Msg: randString(rng)}
		if rng.Intn(2) == 0 {
			r.Buffer, r.Process, r.Package = randString(rng), randString(rng), randString(rng)
		}
		b, err := json.Marshal(&r)
		if err != nil {
			t.Fatal(err)
		}
		if !checkDecode(t, b) {
			t.Fatalf("%q: not on the fast path", b)
		}
	}
}

func FuzzDecodeRecord(f *testing.F) {
	for _, s := range decodeSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		checkDecode(t, line)
	})
}

// FuzzDecodeRecordFields keeps the record layout and fuzzes the raw text
// of the values (numbers and JSON string bodies, escapes included), so
// most inputs stay on the fast path; FuzzDecodeRecord mostly ends in the
// fallback.
func FuzzDecodeRecordFields(f *testing.F) {
	f.Add("1", "1791572737970524000", "73", "-1", "0a1b2c3d", "Tag", "hello", "main")
	f.Add("18446744073709551615", "-9223372036854775808", "255", "10146", `\u00e9`, `\ud83d\ude00`, `\ud83d`, `\ude00\ud83d`)
	f.Add("0", "-0", "0", "2147483648", `a\"b`, `\\`, `\/\b\f\n\r\t`, "\xff\xfe")
	f.Add("01", "1e3", "256", "1.5", `\u12`, `\x`, "\x01", `\uDBFF\uDFFF`)
	f.Fuzz(func(t *testing.T, seq, tm, level, uid, boot, tag, msg, proc string) {
		line := `{"s":` + seq + `,"b":"` + boot + `","t":` + tm + `,"u":` + uid + `,"p":1,"i":2,"l":` + level +
			`,"g":"` + tag + `","m":"` + msg + `","n":"` + proc + `"}`
		checkDecode(t, []byte(line))
	})
}

// the edge-case seeds exercise the fast path too, not only the fallback
func TestDecodeFastShare(t *testing.T) {
	fast := 0
	for _, s := range decodeSeeds {
		if checkDecode(t, []byte(s)) {
			fast++
		}
	}
	if fast < 20 {
		t.Fatalf("only %d of %d seeds on the fast path", fast, len(decodeSeeds))
	}
}

// checkSegments runs the differential check on every line of every
// segment under dir (read-only) and returns lines and fast-path lines.
func checkSegments(t *testing.T, dir string) (lines, fast int) {
	t.Helper()
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.jsonl"))
	more, _ := filepath.Glob(filepath.Join(dir, "*", "seg-*.jsonl"))
	for _, p := range append(segs, more...) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range bytes.Split(b, []byte{'\n'}) {
			if len(ln) == 0 {
				continue
			}
			lines++
			if checkDecode(t, ln) {
				fast++
			}
		}
	}
	return
}

// Every record of the logcat fixtures, stored as the recorder stores it,
// decodes equal to encoding/json, and on the fast path.
func TestDecodeRecordFixtures(t *testing.T) {
	files, _ := filepath.Glob("../testdata/api29/*")
	dir := t.TempDir()
	st, err := Open(dir, "fixtures", Config{IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		asm := &logcat.Assembler{Loc: time.UTC, Year: 2026}
		var recs []*model.Record
		add := func(r *model.Record) {
			if r == nil {
				return
			}
			seq++
			r.Seq, r.Boot = seq, "0a1b2c3d"
			if seq%3 == 0 {
				r.Process, r.Package = "com.example.logtest:remote", "com.example.logtest"
			}
			recs = append(recs, r)
		}
		for _, ln := range strings.Split(string(b), "\n") {
			add(asm.Push(strings.TrimSuffix(ln, "\r")))
		}
		add(asm.Flush())
		if err := st.Append(recs); err != nil {
			t.Fatal(err)
		}
	}
	st.Flush()
	lines, fast := checkSegments(t, st.Dir)
	st.Close()
	if lines < 1000 || fast != lines {
		t.Fatalf("%d lines, %d on the fast path", lines, fast)
	}
	t.Logf("%d fixture records, all on the fast path", lines)
}

// ANDROID_LOG_MCP_DECODE_DIRS (list separated by the OS path list
// separator): device directories of real stores to check, read-only.
func TestDecodeRecordStoreDirs(t *testing.T) {
	env := os.Getenv("ANDROID_LOG_MCP_DECODE_DIRS")
	if env == "" {
		t.Skip("ANDROID_LOG_MCP_DECODE_DIRS not set")
	}
	for _, d := range filepath.SplitList(env) {
		lines, fast := checkSegments(t, d)
		t.Logf("%s: %d lines, %d on the fast path", d, lines, fast)
	}
}

// fixtureLines are the stored lines of the logcat fixtures.
func fixtureLines(b *testing.B) [][]byte {
	files, _ := filepath.Glob("../testdata/api29/*")
	var recs []*model.Record
	for _, f := range files {
		data, _ := os.ReadFile(f)
		asm := &logcat.Assembler{Loc: time.UTC, Year: 2026}
		for _, ln := range strings.Split(string(data), "\n") {
			if r := asm.Push(strings.TrimSuffix(ln, "\r")); r != nil {
				recs = append(recs, r)
			}
		}
	}
	var out [][]byte
	for i, r := range recs {
		r.Seq, r.Boot = uint64(i+1), "0a1b2c3d"
		l, _ := json.Marshal(r)
		out = append(out, l)
	}
	return out
}

func BenchmarkDecodeRecord(b *testing.B) {
	lines := fixtureLines(b)
	for _, c := range []struct {
		name string
		fn   func([]byte, *model.Record) error
	}{{"encoding_json", func(l []byte, r *model.Record) error { return json.Unmarshal(l, r) }}, {"DecodeRecord", DecodeRecord}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			n := 0
			for i := 0; i < b.N; i++ {
				r := new(model.Record)
				if err := c.fn(lines[n], r); err != nil {
					b.Fatal(err)
				}
				if n++; n == len(lines) {
					n = 0
				}
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "records/s")
		})
	}
}
