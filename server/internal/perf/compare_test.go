package perf

import (
	"strings"
	"testing"
	"time"
)

func res(calib float64, ms ...Metric) *Results {
	r := New()
	r.Set(CalibKey, Metric{Value: calib, Unit: "ms", Kind: Info})
	for i, m := range ms {
		r.Set(string(rune('a'+i)), m)
	}
	return r
}

func verdicts(fs []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		v := "ok"
		if f.Fail {
			v = "fail"
		} else if f.Better {
			v = "better"
		}
		out[f.Name] = v
	}
	return out
}

func TestCompareRules(t *testing.T) {
	base := res(100,
		Metric{Value: 10000, Unit: "bytes", Kind: Exact},    // a
		Metric{Value: 10000, Unit: "bytes", Kind: Exact},    // b
		Metric{Value: 100, Unit: "allocs/op", Kind: Allocs}, // c
		Metric{Value: 10000, Unit: "B/op", Kind: Allocs},    // d
		Metric{Value: 100, Unit: "ms", Kind: Time},          // e
		Metric{Value: 100, Unit: "ms", Kind: Time},          // f
		Metric{Value: 1000, Unit: "lines/s", Kind: Rate},    // g
		Metric{Value: 50, Unit: "MiB", Kind: Mem},           // h
		Metric{Value: 1, Unit: "ms", Kind: Time},            // i
		Metric{Value: 5, Unit: "x", Kind: Info},             // j
		Metric{Value: 100, Unit: "ms", Kind: Time},          // k
		Metric{Value: 2, Unit: "allocs/op", Kind: Allocs},   // l
	)
	cur := res(200, // a machine half as fast: timings scale ×2
		Metric{Value: 10250, Unit: "bytes", Kind: Exact},        // a: +2.5 % → fail
		Metric{Value: 9000, Unit: "bytes", Kind: Exact},         // b: −10 % → better
		Metric{Value: 109, Unit: "allocs/op", Kind: Allocs},     // c: +9 % → ok
		Metric{Value: 12000, Unit: "B/op", Kind: Allocs},        // d: +20 % → fail
		Metric{Value: 450, Unit: "ms", Kind: Time},              // e: 4.5× but 2.25× calibrated → ok
		Metric{Value: 550, Unit: "ms", Kind: Time},              // f: 2.75× calibrated → fail
		Metric{Value: 300, Unit: "lines/s", Kind: Rate},         // g: calibrated base 500 / 2.5 = 200 → ok
		Metric{Value: 70, Unit: "MiB", Kind: Mem},               // h: +40 % → ok
		Metric{Value: 6, Unit: "ms", Kind: Time},                // i: 3× calibrated but < +5 ms → ok
		Metric{Value: 500, Unit: "x", Kind: Info},               // j: info → ok
		Metric{Value: 150, Unit: "ms", Kind: Time, Budget: 120}, // k: within factor, over budget → fail
		Metric{Value: 3, Unit: "allocs/op", Kind: Allocs},       // l: +1 alloc (≤ abs slack) → ok
	)
	want := map[string]string{"a": "fail", "b": "better", "c": "ok", "d": "fail", "e": "ok", "f": "fail", "g": "ok",
		"h": "ok", "i": "ok", "j": "ok", "k": "fail", "l": "ok", CalibKey: "ok"}
	got := verdicts(Compare(base, cur, DefaultRules))
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %s, want %s", k, got[k], v)
		}
	}
	// a metric that disappeared fails; a new one does not
	delete(cur.Metrics, "c")
	cur.Set("zz", Metric{Value: 1, Kind: Exact})
	fs := Compare(base, cur, DefaultRules)
	if v := verdicts(fs); v["c"] != "fail" || v["zz"] != "" {
		t.Errorf("missing/new metric: %v", v)
	}
	if !Failed(fs) {
		t.Error("Failed = false")
	}
}

func TestRetime(t *testing.T) {
	in := "--------- beginning of main\n         1791572202.589379 10146  6713  6713 V LogTest : hello\ncontinuation\n 1791572203.000001  1000   1   1 I x: y"
	out := Retime(in, 10*time.Second)
	want := "--------- beginning of main\n         1791572212.589379 10146  6713  6713 V LogTest : hello\ncontinuation\n 1791572213.000001  1000   1   1 I x: y"
	if out != want {
		t.Fatalf("Retime:\n%s\nwant\n%s", out, want)
	}
	if f, l := Span(in); f != 1791572202 || l != 1791572203 {
		t.Fatalf("Span = %d %d", f, l)
	}
	var got []string
	Copies(in, 2, time.Minute, func(k int, c string) error {
		got = append(got, strings.Fields(strings.Split(c, "\n")[1])[0])
		return nil
	})
	if len(got) != 2 || got[0] != "1791572202.589379" || got[1] != "1791572263.589379" {
		t.Fatalf("Copies: %q", got)
	}
}
