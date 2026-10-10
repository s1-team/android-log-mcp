package logcat

import (
	"strings"
	"testing"
	"time"
)

func TestParseLine_epochUidUsec(t *testing.T) {
	l := ParseLine("         1791570419.946498  1000   191  1987 W libc    : Unable to set property \"ctl.interface_start\" to \"aidl/activity\": PROP_ERROR", nil, 0)
	if !l.OK || l.UID != 1000 || l.PID != 191 || l.TID != 1987 || l.Level != 'W' || l.Tag != "libc" {
		t.Fatalf("%+v", l)
	}
	if l.TimeNs != 1791570419946498000 {
		t.Errorf("ts %d", l.TimeNs)
	}
	if !strings.HasPrefix(l.Msg, "Unable to set") || !strings.HasSuffix(l.Msg, ": PROP_ERROR") {
		t.Errorf("msg %q", l.Msg)
	}
}

func TestParseLine_variants(t *testing.T) {
	cases := []struct {
		in       string
		uid, pid int
		tag, msg string
		lv       byte
	}{
		{"1791570419.316956  root     1     1 I init    : hello", 0, 1, "init", "hello", 'I'},
		{"1791570419.316956 10123  4321  4333 D OkHttp  : --> GET https://x/y", 10123, 4321, "OkHttp", "--> GET https://x/y", 'D'},
		{"1791570419.316956  1000  1880  1880 I am_proc_start: [0,4321,10123,com.foo,next-top-activity,{com.foo/com.foo.Main}]", 1000, 1880, "am_proc_start", "[0,4321,10123,com.foo,next-top-activity,{com.foo/com.foo.Main}]", 'I'},
		{"1791570419.316956  1880  1880 E AndroidRuntime: FATAL EXCEPTION: main", -1, 1880, "AndroidRuntime", "FATAL EXCEPTION: main", 'E'},
		{"1791570419.316956  1000  1880  1880 I My Tag With Spaces: x", 1000, 1880, "My Tag With Spaces", "x", 'I'},
		{"1791570419.316956  1000  1880  1880 I EmptyMsg: ", 1000, 1880, "EmptyMsg", "", 'I'},
		{"1791570419.316956  1000  1880  1880 A DEBUG   : *** *** ***", 1000, 1880, "DEBUG", "*** *** ***", 'F'},
	}
	for _, c := range cases {
		l := ParseLine(c.in, nil, 0)
		if !l.OK || l.UID != c.uid || l.PID != c.pid || l.Tag != c.tag || l.Msg != c.msg || byte(l.Level) != c.lv {
			t.Errorf("%q => %+v", c.in, l)
		}
	}
}

func TestParseLine_threadtimeFallback(t *testing.T) {
	loc := time.FixedZone("x", 8*3600)
	l := ParseLine("10-10 01:20:00.123  1000  1880  1880 I Tag: m", loc, 2026)
	if !l.OK || time.Unix(0, l.TimeNs).In(loc).Format("2006-01-02 15:04:05.000") != "2026-10-10 01:20:00.123" {
		t.Fatalf("%+v", l)
	}
}

func TestParseLine_dividerAndGarbage(t *testing.T) {
	if l := ParseLine("--------- beginning of crash", nil, 0); l.Divider != "crash" {
		t.Error(l)
	}
	if l := ParseLine("--------- switch to events", nil, 0); l.Divider != "events" {
		t.Error(l)
	}
	if l := ParseLine("random garbage", nil, 0); l.OK {
		t.Error("garbage parsed")
	}
}

func TestAssembler_multiline(t *testing.T) {
	in := `--------- beginning of crash
1791570419.100000 10123  4321  4321 E AndroidRuntime: FATAL EXCEPTION: main
1791570419.100000 10123  4321  4321 E AndroidRuntime: Process: com.foo, PID: 4321
1791570419.100000 10123  4321  4321 E AndroidRuntime: java.lang.IllegalStateException: boom
1791570419.100000 10123  4321  4321 E AndroidRuntime: 	at com.foo.Main.onCreate(Main.kt:10)
--------- switch to main
1791570419.100000 10123  4321  4321 E AndroidRuntime: separate (other buffer)
1791570419.200000 10123  4321  4321 I Foo     : one
1791570419.200000 10123  4321  4321 I Foo     : one
1791570419.300000 10123  4321  4322 I Foo     : other thread`
	a := &Assembler{}
	var recs []string
	for _, line := range strings.Split(in, "\n") {
		if r := a.Push(line); r != nil {
			recs = append(recs, r.Buffer+"|"+r.Msg)
		}
	}
	if r := a.Flush(); r != nil {
		recs = append(recs, r.Buffer+"|"+r.Msg)
	}
	want := []string{
		"crash|FATAL EXCEPTION: main\nProcess: com.foo, PID: 4321\njava.lang.IllegalStateException: boom\n\tat com.foo.Main.onCreate(Main.kt:10)",
		"main|separate (other buffer)",
		"main|one\none", // identical header => merged (known limitation, documented)
		"main|other thread",
	}
	if strings.Join(recs, "\n#\n") != strings.Join(want, "\n#\n") {
		t.Errorf("got:\n%s", strings.Join(recs, "\n#\n"))
	}
}

func TestEventFields(t *testing.T) {
	f := EventFields("[0,4321,10123,com.foo,activity,{com.foo/com.foo.Main}]", 0)
	if len(f) != 6 || f[3] != "com.foo" || f[5] != "{com.foo/com.foo.Main}" {
		t.Errorf("%q", f)
	}
	f = EventFields("[0,1234,com.foo,952745540,Input dispatching timed out (a, b, c)]", 5)
	if len(f) != 5 || f[4] != "Input dispatching timed out (a, b, c)" {
		t.Errorf("%q", f)
	}
}

func TestResumeTime(t *testing.T) {
	if s := FormatResumeTime(1791570419000123000); s != "1791570419.000123" {
		t.Error(s)
	}
	if a := Args("1.000001"); a[len(a)-1] != "1.000001" || a[len(a)-2] != "-T" {
		t.Error(a)
	}
}

// logprint prints the AID name instead of the number when it is ≤ 5 chars.
func TestShortUIDNames(t *testing.T) {
	for name, uid := range map[string]int{"root": 0, "radio": 1001, "llkd": 1070, "lmkd": 1069, "shell": 2000} {
		l := ParseLine("         1791614446.168224  "+name+"  1910  1910 W livelock: sample 3.490s", time.UTC, 2026)
		if !l.OK || l.UID != uid {
			t.Errorf("%s: ok=%v uid=%d", name, l.OK, l.UID)
		}
	}
}
