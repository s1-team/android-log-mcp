package analysis

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Issue #225 PR1: evidence, content key, merge, open-fact lookup.

func TestContentKeyIgnoresTimesPidsDurationsAndHashes(t *testing.T) {
	t.Parallel()
	base := "Input dispatching timed out (Waited 5001ms for MotionEvent) at 2026-10-17 12:00:01.123 pid 4321 tid=77 0x7ef7d16d7400 @1a2b3c4d"
	same := []string{
		"Input dispatching timed out (Waited 5002ms for MotionEvent) at 2026-10-17T13:14:15 pid 99 tid=1 0xdeadbeef @ffee1122",
		"Input   dispatching timed out (Waited 4999ms for MotionEvent)\n at 10-17 12:00:01.5 pid: 5 TID 6 0x1 @abcdef0",
		"Input dispatching timed out (Waited 5.001s for MotionEvent) at 12:00:01 (pid 1) tid=2 0x00 @00000000",
		"Input dispatching timed out (Waited 5001ms for MotionEvent) at 1791572753 pid 4321 tid=77 0x7ef7d16d7400 @1a2b3c4d",
		"Input dispatching timed out (Waited 5001ms for MotionEvent) at 1791572753.236 pid 4321 tid=77 0x7ef7d16d7400 @1a2b3c4d\r\n",
	}
	want := ContentKey("events", "com.foo", base)
	if len(want) != 16 {
		t.Fatalf("key %q is not 16 hex digits", want)
	}
	for _, s := range same {
		if got := ContentKey("events", "com.foo", s); got != want {
			t.Errorf("%q: key %s, want %s (normalized %q vs %q)", s, got, want, NormalizeReason(s), NormalizeReason(base))
		}
	}
	for name, got := range map[string]string{
		"other reason":  ContentKey("events", "com.foo", "Broadcast of Intent"),
		"other process": ContentKey("events", "com.bar", base),
		"other class":   ContentKey("logcat", "com.foo", base),
		"case matters":  ContentKey("events", "com.foo", strings.ToUpper(base)),
	} {
		if got == want {
			t.Errorf("%s: same key", name)
		}
	}
	if n := len(NormalizeReason(strings.Repeat("x", 2000))); n != 512 {
		t.Errorf("cut to %d bytes, want 512", n)
	}
}

// The anr_broadcast fixtures: am_anr field 5, the block's Reason: and the
// DropBox Subject normalize to one text, so the three keys agree.
func TestContentKeyAnrBroadcastFixtures(t *testing.T) {
	t.Parallel()
	read := func(n string) string {
		b, err := os.ReadFile(filepath.Join("..", "testdata", "api29", n))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	drop := read("dropbox-data_app_anr.txt")
	var subject, event, reason string
	for _, e := range ParseDropbox(drop, time.UTC) {
		subject = headerField(e.Body, "Subject")
	}
	for _, ln := range strings.Split(read("anr_broadcast.logcat.txt"), "\n") {
		if i := strings.Index(ln, " I am_anr  : ["); i >= 0 {
			f := strings.SplitN(strings.TrimSuffix(ln[i+len(" I am_anr  : ["):], "]"), ",", 5)
			event = f[4]
		}
		if i := strings.Index(ln, "ActivityManager: Reason: "); i >= 0 {
			reason = ln[i+len("ActivityManager: Reason: "):]
		}
	}
	if subject == "" || event == "" || reason == "" {
		t.Fatalf("fixtures: %q %q %q", subject, event, reason)
	}
	const p = "com.example.logtest"
	k := ContentKey("events", p, event)
	if ContentKey("events", p, reason) != k || ContentKey("events", p, subject) != k {
		t.Errorf("keys differ: event %q, block %q, DropBox %q", event, reason, subject)
	}
}

func TestEvidenceIdentity(t *testing.T) {
	t.Parallel()
	ev := func(s string, q uint64, tm int64, ck string) Evidence { return Evidence{S: s, Q: q, T: tm, CK: ck} }
	for _, tc := range []struct {
		name string
		a, b Evidence
		same bool
	}{
		{"same class and seq", ev("logcat:system", 5, 1, "x"), ev("logcat:crash", 5, 2, "y"), true},
		{"other seq, same ck and time", ev("logcat:system", 5, 1, "x"), ev("logcat:system", 6, 1, "x"), false},
		{"no seq: same time and ck", ev("events:am_anr", 0, 7, "k"), ev("events:am_anr", 0, 7, "k"), true},
		{"no seq: ck alone is no repeat", ev("events:am_anr", 0, 7, "k"), ev("events:am_anr", 0, 9, "k"), false},
		{"no seq: other ck", ev("events:am_anr", 0, 7, "k"), ev("events:am_anr", 0, 7, "j"), false},
		{"other class", ev("events:am_anr", 5, 1, "x"), ev("logcat:system", 5, 1, "x"), false},
		{"synthesized has no identity", ev("events:am_anr", 0, 0, ""), ev("events:am_anr", 0, 0, ""), false},
		{"dropbox body key", ev("dropbox:data_app_anr", 0, 7, "body"), ev("dropbox:data_app_anr", 0, 7, "body"), true},
		{"seq vs none", ev("logcat:system", 5, 1, "x"), ev("logcat:system", 0, 1, "x"), false},
	} {
		if got := tc.a.Same(tc.b); got != tc.same || tc.b.Same(tc.a) != tc.same {
			t.Errorf("%s: Same = %v, want %v", tc.name, got, tc.same)
		}
	}
	c := &Crash{}
	if !c.AddEvidence(ev("events:am_anr", 5, 1, "x")) || c.AddEvidence(ev("events:am_anr", 5, 1, "x")) || len(c.Ev) != 1 || len(c.Sources) != 1 {
		t.Errorf("AddEvidence: %+v", c)
	}
}

func anrFact(id string, sec int64, pid int, ev ...Evidence) *Crash {
	c := &Crash{ID: id, Kind: "anr", Boot: "b1", TimeNs: sec * 1e9, Seq: uint64(sec), PID: pid, Process: "com.foo", Headline: "r"}
	for _, e := range ev {
		c.AddEvidence(e)
	}
	return c
}

func evAnr(q uint64) Evidence { return Evidence{S: "events:am_anr", Q: q, T: int64(q) * 1e9, CK: "e"} }
func evBlock(q uint64) Evidence {
	return Evidence{S: "logcat:system", Q: q, T: int64(q) * 1e9, CK: "b"}
}
func evDrop(k string) Evidence { return Evidence{S: "dropbox:data_app_anr", T: 5, CK: k} }

func TestMergeFact(t *testing.T) {
	t.Parallel()
	t.Run("idempotent", func(t *testing.T) {
		l := anrFact("anr1", 100, 7, evAnr(100), evBlock(105), evDrop("k"))
		l.Traces, l.CPU, l.Body = "tr", "cpu", "body"
		if got := MergeFact(l, l, MergeMode{}); !reflect.DeepEqual(got, l) {
			t.Errorf("merge(l, l) != l:\n%+v\n%+v", got, l)
		}
	})
	t.Run("union of evidence, sources derived", func(t *testing.T) {
		l := anrFact("anr1", 100, 7, evAnr(100), evDrop("k"))
		in := anrFact("", 105, 7, evBlock(105))
		got := MergeFact(l, in, MergeMode{Looked: true})
		if len(got.Ev) != 3 || got.ID != "anr1" || !reflect.DeepEqual(got.Sources, []string{"events:am_anr", "dropbox:data_app_anr", "logcat:system"}) {
			t.Errorf("%+v", got)
		}
		// an ID-carried snapshot keeps its own order, then the stored extras
		got = MergeFact(l, in, MergeMode{})
		if !reflect.DeepEqual(got.Sources, []string{"logcat:system", "events:am_anr", "dropbox:data_app_anr"}) {
			t.Errorf("sources %v", got.Sources)
		}
	})
	t.Run("an ANR keeps its anchor and headline", func(t *testing.T) {
		l := anrFact("anr1", 100, 7, evAnr(100))
		in := anrFact("", 105, 7, evBlock(105))
		in.Headline, in.Body = "block reason", "block"
		got := MergeFact(l, in, MergeMode{Looked: true})
		if got.TimeNs != 100e9 || got.Seq != 100 || got.Headline != "r" || got.Body != "block" {
			t.Errorf("%+v", got)
		}
	})
	t.Run("fill if empty", func(t *testing.T) {
		l := anrFact("anr1", 100, 7, evAnr(100))
		l.Traces, l.CPU, l.Tombstone, l.Dropbox, l.Location = "tr", "cpu", "tomb", "drop", "loc"
		in := anrFact("anr1", 100, 7, evAnr(100), evBlock(105))
		got := MergeFact(l, in, MergeMode{})
		if got.Traces != "tr" || got.CPU != "cpu" || got.Tombstone != "tomb" || got.Dropbox != "drop" || got.Location != "loc" {
			t.Errorf("%+v", got)
		}
		if got = MergeFact(l, in, MergeMode{NoDropboxFill: true}); got.Dropbox != "" || got.Traces != "tr" {
			t.Errorf("NoDropboxFill: %+v", got)
		}
	})
	t.Run("events-only snapshot never replaces logcat text of a java crash", func(t *testing.T) {
		l := &Crash{ID: "cr1", Kind: "java", PID: 7, TimeNs: 50, Seq: 9, Headline: "real", Body: "stack", Signature: "sig", Thread: "main"}
		l.AddEvidence(Evidence{S: "logcat:crash", Q: 9, CK: "a"})
		in := &Crash{Kind: "java", PID: 7, TimeNs: 40, Seq: 8, Headline: "exc: msg", Body: "only the am_crash event", Signature: "x@y"}
		in.AddEvidence(Evidence{S: "events:am_crash", Q: 8, CK: "b"})
		got := MergeFact(l, in, MergeMode{Looked: true})
		if got.Body != "stack" || got.Headline != "real" || got.Signature != "sig" || got.TimeNs != 50 || got.Seq != 9 || len(got.Ev) != 2 {
			t.Errorf("%+v", got)
		}
		// the other way round the logcat snapshot upgrades an events-only fact
		l2 := &Crash{ID: "cr1", Kind: "java", PID: 7, TimeNs: 40, Seq: 8, Headline: "exc: msg", Body: "only the am_crash event"}
		l2.AddEvidence(Evidence{S: "events:am_crash", Q: 8, CK: "b"})
		in2 := &Crash{Kind: "java", PID: 7, TimeNs: 50, Seq: 9, Headline: "real", Body: "stack", Signature: "sig"}
		in2.AddEvidence(Evidence{S: "logcat:crash", Q: 9, CK: "a"})
		got = MergeFact(l2, in2, MergeMode{Looked: true})
		if got.Body != "stack" || got.Headline != "real" || got.TimeNs != 50 || got.Seq != 9 {
			t.Errorf("%+v", got)
		}
	})
	t.Run("legacy stored version", func(t *testing.T) {
		l := &Crash{ID: "anr1", Kind: "anr", Boot: "b1", PID: 7, TimeNs: 100e9, Seq: 100, Sources: []string{"events:am_anr"}}
		in := anrFact("", 105, 7, evBlock(105))
		got := MergeFact(l, in, MergeMode{Looked: true})
		if len(got.Ev) != 2 || got.Ev[0] != (Evidence{S: "events:am_anr"}) || got.Ev[1].Q != 105 {
			t.Errorf("%+v", got.Ev)
		}
		// the real entry replaces its identity-less placeholder
		in2 := anrFact("", 100, 7, evAnr(100))
		got = MergeFact(l, in2, MergeMode{})
		if len(got.Ev) != 1 || got.Ev[0].Q != 100 {
			t.Errorf("%+v", got.Ev)
		}
	})
	t.Run("neither argument is modified", func(t *testing.T) {
		l := anrFact("anr1", 100, 7, evAnr(100))
		in := anrFact("", 105, 7, evBlock(105))
		lc, ic := *l, *in
		lc.Ev, ic.Ev = append([]Evidence(nil), l.Ev...), append([]Evidence(nil), in.Ev...)
		lc.Sources, ic.Sources = append([]string(nil), l.Sources...), append([]string(nil), in.Sources...)
		MergeFact(l, in, MergeMode{Looked: true})
		if !reflect.DeepEqual(*l, lc) || !reflect.DeepEqual(*in, ic) {
			t.Error("modified")
		}
	})
}

func TestFindOpenFact(t *testing.T) {
	t.Parallel()
	f1 := anrFact("anr1", 1000, 7, evAnr(1000))
	f2 := anrFact("anr2", 1200, 7, evAnr(1200))
	in := func(sec int64, e Evidence) *Crash { c := anrFact("", sec, 7, e); c.Boot = "b1"; return c }
	t.Run("newest open fact within the window that lacks the class", func(t *testing.T) {
		got, rep := FindOpenFact([]*Crash{f2, f1}, in(1210, evBlock(1210)), nil, nil)
		if got != f2 || rep {
			t.Fatalf("%v %v", got, rep)
		}
		// f2 is complete for events: a second am_anr goes to no one
		if got, _ := FindOpenFact([]*Crash{f2, f1}, in(1210, evAnr(1210)), nil, nil); got != nil {
			t.Fatalf("paired with %s", got.ID)
		}
	})
	t.Run("window", func(t *testing.T) {
		for _, tc := range []struct {
			gap  int64
			want bool
		}{{119, true}, {121, true}, {600 - 1, true}, {600, true}, {601, false}} { // the 120 s window is gone (PR2): the cap is anrEvidenceCap
			got, _ := FindOpenFact([]*Crash{f1}, in(1000+tc.gap, evBlock(uint64(1000+tc.gap))), nil, nil)
			if (got != nil) != tc.want {
				t.Errorf("+%d s: %v", tc.gap, got)
			}
		}
	})
	t.Run("repeat", func(t *testing.T) {
		got, rep := FindOpenFact([]*Crash{f1}, in(1000, evAnr(1000)), nil, nil)
		if got != f1 || !rep {
			t.Fatalf("%v %v", got, rep)
		}
	})
	t.Run("other pid, boot and kind", func(t *testing.T) {
		x := in(1010, evBlock(1010))
		x.PID = 8
		if got, _ := FindOpenFact([]*Crash{f1}, x, nil, nil); got != nil {
			t.Error("pid")
		}
		x = in(1010, evBlock(1010))
		x.Boot = "b2"
		if got, _ := FindOpenFact([]*Crash{f1}, x, nil, nil); got != nil {
			t.Error("boot")
		}
		x = in(1010, evBlock(1010))
		x.Kind = "java"
		if got, _ := FindOpenFact([]*Crash{f1}, x, nil, nil); got != nil {
			t.Error("kind")
		}
	})
	t.Run("compat veto", func(t *testing.T) {
		if got, _ := FindOpenFact([]*Crash{f1}, in(1010, evBlock(1010)), nil, func(*Crash) bool { return false }); got != nil {
			t.Error("paired against the veto")
		}
	})
	t.Run("a clock jump after the fact is bridged", func(t *testing.T) {
		jumps := []ClockShift{{Seq: 1001, DeltaNs: -86400e9}}
		x := in(1010-86400, evBlock(1010))
		if got, _ := FindOpenFact([]*Crash{f1}, x, jumps, nil); got != f1 {
			t.Error("not bridged")
		}
		if got, _ := FindOpenFact([]*Crash{f1}, x, nil, nil); got != nil {
			t.Error("paired across a day without a jump")
		}
	})
	t.Run("dropbox-only facts are left to AdoptDropbox", func(t *testing.T) {
		d := anrFact("anr3", 1000, 7, evDrop("k"))
		if got, _ := FindOpenFact([]*Crash{d}, in(1010, evBlock(1010)), nil, nil); got != nil {
			t.Error("took a DropBox-only fact")
		}
	})
	t.Run("java within 15 s, any class", func(t *testing.T) {
		j := &Crash{ID: "cr1", Kind: "java", Boot: "b1", PID: 7, TimeNs: 100e9, Seq: 3}
		j.AddEvidence(Evidence{S: "logcat:crash", Q: 3, CK: "a"})
		x := &Crash{Kind: "java", Boot: "b1", PID: 7, TimeNs: 110e9, Seq: 4}
		x.AddEvidence(Evidence{S: "events:am_crash", Q: 4, CK: "b"})
		if got, _ := FindOpenFact([]*Crash{j}, x, nil, nil); got != j {
			t.Error("not paired")
		}
		x.TimeNs = 116e9
		if got, _ := FindOpenFact([]*Crash{j}, x, nil, nil); got != nil {
			t.Error("paired beyond 15 s")
		}
	})
	t.Run("native by process and class", func(t *testing.T) {
		n := &Crash{ID: "cr1", Kind: "native", Boot: "b1", PID: 7736, Process: "com.example.logtest", TimeNs: 100e9, Seq: 3}
		n.AddEvidence(Evidence{S: "logcat:tombstone", Q: 3, CK: "a"})
		ev := &Crash{Kind: "native", Boot: "b1", Process: "com.example.logtest", TimeNs: 110e9, Seq: 4}
		ev.AddEvidence(Evidence{S: "events:am_crash", Q: 4, CK: "b"})
		if got, _ := FindOpenFact([]*Crash{n}, ev, nil, nil); got != n {
			t.Error("am_crash not paired with the tombstone fact")
		}
		n.AddEvidence(Evidence{S: "events:am_crash", Q: 4, CK: "b"})
		ev2 := &Crash{Kind: "native", Boot: "b1", Process: "com.example.logtest", TimeNs: 112e9, Seq: 6}
		ev2.AddEvidence(Evidence{S: "events:am_crash", Q: 6, CK: "c"})
		if got, _ := FindOpenFact([]*Crash{n}, ev2, nil, nil); got != nil {
			t.Error("a second am_crash joined a fact that has one")
		}
	})
}

func TestDetectorRecordsEvidence(t *testing.T) {
	t.Parallel()
	r := newJumpRig(t)
	r.eventAt(11, 1000, 4321)
	r.blockAt(12, 1005, 4321)
	r.eventAt(11, 1000, 4321) // the same record again
	if len(r.seen) != 1 {
		t.Fatalf("%d crashes", len(r.seen))
	}
	c := r.seen[0]
	if len(c.Ev) != 2 {
		t.Fatalf("%+v", c.Ev)
	}
	e, b := c.Ev[0], c.Ev[1]
	if e.S != "events:am_anr" || e.Q != 11 || e.T != 1000e9 || b.S != "logcat:crash" || b.Q != 12 || b.T != 1005e9 {
		t.Errorf("%+v", c.Ev)
	}
	if e.CK != ContentKey("events", "com.foo", "Input dispatching timed out") || b.CK != ContentKey("logcat", "com.foo", "Input dispatching timed out") {
		t.Errorf("ck %+v", c.Ev)
	}
}

// A Lifetime resolver splits the detector's in-memory pairing too: the same
// pid in two lifetimes is two crashes, an anchor that only resolves to the
// new lifetime once its start line is known still pairs.
func TestDetectorLifetimeVeto(t *testing.T) {
	t.Parallel()
	life := map[uint64]string{}
	feed := func(r *jumpRig) {
		r.d.Lifetime = func(pid int, devNs int64, seq uint64) string { return life[seq] }
	}
	t.Run("different lifetimes", func(t *testing.T) {
		r := newJumpRig(t)
		feed(r)
		life[1], life[2] = "L1", "L2"
		r.eventAt(1, 1000, 4321)
		r.blockAt(2, 1005, 4321)
		if n := len(anrs(r)); n != 2 {
			t.Fatalf("%d crashes, want 2", n)
		}
	})
	t.Run("anchor re-resolved", func(t *testing.T) {
		r := newJumpRig(t)
		feed(r)
		life[11], life[12] = "L1", "L2"
		r.eventAt(11, 1000, 4321)
		life[11] = "L2" // the start line arrived: the anchor resolves to the new lifetime
		r.blockAt(12, 1005, 4321)
		if n := len(anrs(r)); n != 1 {
			t.Fatalf("%d crashes, want 1", n)
		}
	})
	t.Run("unknown side: names decide", func(t *testing.T) {
		r := newJumpRig(t)
		feed(r)
		r.eventAt(21, 1000, 4321)
		r.blockAt(22, 1005, 4321)
		if n := len(anrs(r)); n != 1 {
			t.Fatalf("%d crashes, want 1", n)
		}
	})
}
