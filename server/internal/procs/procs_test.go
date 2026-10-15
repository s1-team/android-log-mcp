package procs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

func ev(ns int64, pid int, tag, msg string) *model.Record {
	return &model.Record{TimeNs: ns, PID: pid, TID: pid, Level: 'I', Tag: tag, Msg: msg, UID: 1000}
}

func TestLifecycleAndAnnotate(t *testing.T) {
	tr := New()
	tr.Reset("b1")
	tr.SetPackages(ParsePackagesU("package:com.foo uid:10123\npackage:com.bar uid:10124\npackage:android uid:1000\npackage:com.android.settings uid:1000\n"))
	var changes []Proc
	tr.OnChange = func(p Proc) { changes = append(changes, p) }

	// app logs slightly before system_server's am_proc_start (fork race)
	early := &model.Record{TimeNs: 9_990_000_000, PID: 4321, UID: 10123, Tag: "app", Msg: "early"}

	tr.Observe(ev(10e9, 1880, "am_proc_start", "[0,4321,10123,com.foo,next-top-activity,{com.foo/com.foo.Main}]"))
	tr.Observe(ev(10e9+1e6, 1880, "ActivityManager", "Start proc 4321:com.foo/u0a123 for next-top-activity {com.foo/com.foo.Main}")) // dup
	tr.Observe(ev(11e9, 1880, "am_proc_start", "[0,4330,10123,com.foo:push,service,{com.foo/com.foo.PushService}]"))

	if !tr.Annotate(early) || early.Process != "com.foo" || early.Package != "com.foo" {
		t.Fatalf("early: %+v", early)
	}
	r := &model.Record{TimeNs: 12e9, PID: 4330, UID: 10123}
	tr.Annotate(r)
	if r.Process != "com.foo:push" || r.Package != "com.foo" {
		t.Fatalf("push: %+v", r)
	}
	tr.Observe(ev(20e9, 4321, "AndroidRuntime", "FATAL EXCEPTION: main"))
	tr.Observe(ev(20e9+5e6, 1880, "am_proc_died", "[0,4321,com.foo,900,19]"))
	// pid reuse for a different app
	tr.Observe(ev(30e9, 1880, "am_proc_start", "[0,4321,10124,com.bar,service,{com.bar/com.bar.S}]"))

	// a record from the old lifetime still resolves to com.foo
	old := &model.Record{TimeNs: 19e9, PID: 4321, UID: 10123}
	tr.Annotate(old)
	if old.Process != "com.foo" {
		t.Fatalf("old: %+v", old)
	}
	nw := &model.Record{TimeNs: 31e9, PID: 4321, UID: 10124}
	tr.Annotate(nw)
	if nw.Process != "com.bar" || nw.Package != "com.bar" {
		t.Fatalf("new: %+v", nw)
	}
	dead := tr.Lookup(4321, 19e9)
	if dead == nil || dead.Name != "com.foo" || dead.Alive() || !dead.Crashed || dead.EndNs != 20e9+5e6 {
		t.Fatalf("dead: %+v", dead)
	}
	if len(changes) < 4 {
		t.Fatalf("changes %d", len(changes))
	}
	// unknown pid: package from uid
	u := &model.Record{TimeNs: 1, PID: 7777, UID: 10124}
	if tr.Annotate(u) || u.Package != "com.bar" {
		t.Fatalf("unknown: %+v", u)
	}
}

func TestTextLifecycle(t *testing.T) {
	tr := New()
	tr.Reset("b")
	tr.Observe(ev(1e9, 1, "ActivityManager", "Start proc 555:com.x:remote/u0a77 for service {com.x/com.x.S}"))
	p := tr.Lookup(555, 2e9)
	if p == nil || p.UID != 10077 || p.Name != "com.x:remote" || p.Package != "com.x" {
		t.Fatalf("%+v", p)
	}
	tr.Observe(ev(3e9, 1, "ActivityManager", "Killing 555:com.x:remote/u0a77 (adj 905): empty #17"))
	if p := tr.Lookup(555, 4e9); p.Alive() || p.EndReason != "killed: empty #17" {
		t.Fatalf("%+v", p)
	}
	tr.Observe(ev(5e9, 1, "ActivityManager", "Start proc 556:com.y/u0a78 for activity {com.y/com.y.M}"))
	tr.Observe(ev(6e9, 1, "ActivityManager", "Process com.y (pid 556) has died: fg  TOP"))
	if p := tr.Lookup(556, 7e9); p.Alive() {
		t.Fatal("should be dead")
	}
}

func TestParsePS(t *testing.T) {
	out := "PID   UID NAME\n    1     0 init\n  191  1000 logd\n 4321 10123 com.foo\n"
	ps := ParsePS(out)
	if len(ps) != 3 || ps[2].UID != 10123 || ps[2].Name != "com.foo" {
		t.Fatalf("%+v", ps)
	}
	legacy := "USER      PID   PPID  VSZ  RSS  WCHAN            ADDR S NAME\nroot        1     0  1 1 0 0 S init\nu0_a123  4321   300 1 1 0 0 S com.foo\n"
	ps = ParsePS(legacy)
	if len(ps) != 2 || ps[1].UID != 10123 || ps[0].UID != 0 {
		t.Fatalf("%+v", ps)
	}
	tr := New()
	tr.Seed(ps, 1)
	if p := tr.Lookup(4321, 5); p == nil || p.Package != "com.foo" {
		t.Fatalf("%+v", p)
	}
}

func TestParsePackagesU(t *testing.T) {
	m := ParsePackagesU("package:com.foo uid:10123\npackage:com.shared.a uid:10200\npackage:com.shared.b uid:10200\npackage:com.multi uid:10300,1010300\n")
	if len(m[10200]) != 2 || m[10123][0] != "com.foo" || len(m[10300]) != 1 {
		t.Fatalf("%v", m)
	}
	if ParseUserUID("u10_a5") != 1010005 || ParseUserUID("u0a123") != 10123 || ParseUserUID("system") != 1000 {
		t.Fatal("uid parse")
	}
}

func seqEv(seq uint64, ns int64, pid int, tag, msg string) *model.Record {
	r := ev(ns, pid, tag, msg)
	r.Seq = seq
	return r
}

func lineAt(seq uint64, ns int64, pid int) *model.Record {
	return &model.Record{Seq: seq, TimeNs: ns, PID: pid, UID: -1}
}

// Issue #55 C3: com.foo owns pid 50 at 1000 s and dies at 1010 s; the clock
// is set back; com.bar gets pid 50 at 400 s.
func seqTimeline() *Tracker {
	tr := New()
	tr.Reset("b")
	tr.Observe(seqEv(1, 1000e9, 1, "am_proc_start", "[0,50,10001,com.foo,service,{com.foo/com.foo.S}]"))
	tr.Observe(seqEv(2, 1010e9, 1, "am_proc_died", "[0,50,com.foo,900,19]"))
	tr.ClockJump(3)
	tr.Observe(seqEv(3, 400e9, 1, "am_proc_start", "[0,50,10002,com.bar,service,{com.bar/com.bar.S}]"))
	return tr
}

func TestLookupSeqAfterBackJump(t *testing.T) {
	tr := seqTimeline()
	for _, c := range []struct {
		seq  uint64
		ns   int64
		want string
	}{
		{4, 410e9, "com.bar"},
		{5, 1005e9, "com.bar"}, // by time it is com.foo's
		{6, 1600e9, "com.bar"}, // by time nobody's
		{2, 1005e9, "com.foo"}, // before the jump
	} {
		p := tr.LookupSeq(50, c.ns, c.seq)
		if p == nil || p.Name != c.want {
			t.Errorf("seq %d ns %d: %+v, want %s", c.seq, c.ns, p, c.want)
		}
	}
}

func TestForkRaceSlackOnlyInSameEpoch(t *testing.T) {
	start := func(jump bool) *Tracker {
		tr := New()
		tr.Reset("b")
		tr.Observe(seqEv(1, 100e9, 1, "am_proc_start", "[0,70,10001,com.old,service,{com.old/com.old.S}]"))
		tr.Observe(seqEv(2, 105e9, 1, "am_proc_died", "[0,70,com.old,900,19]"))
		if jump {
			tr.ClockJump(5)
		}
		tr.Observe(seqEv(5, 200e9, 1, "am_proc_start", "[0,70,10002,com.new,service,{com.new/com.new.S}]"))
		return tr
	}
	// line 1.5 s before the start line, no jump: new owner
	if p := start(false).LookupSeq(70, 198_500_000_000, 4); p == nil || p.Name != "com.new" {
		t.Fatalf("same epoch: %+v", p)
	}
	// same device time gap, but a jump lies between: previous owner's grace
	// does not apply either (it ended long before): none
	if p := start(true).LookupSeq(70, 198_500_000_000, 4); p != nil && p.Name == "com.new" {
		t.Fatalf("across a jump the slack must not apply: %+v", p)
	}
}

func TestEndedAndReusedAcrossBackJump(t *testing.T) {
	tr := New()
	tr.Reset("b")
	tr.Observe(seqEv(1, 1000e9, 1, "am_proc_start", "[0,80,10001,com.foo,service,{com.foo/com.foo.S}]"))
	tr.ClockJump(2)
	// pid reused after a back jump, no died line: the old lifetime ends
	// at its own start, not before it
	tr.Observe(seqEv(2, 300e9, 1, "am_proc_start", "[0,80,10002,com.bar,service,{com.bar/com.bar.S}]"))
	old := tr.LookupSeq(80, 1001e9, 1)
	if old == nil || old.Name != "com.foo" || old.EndSeq != 2 || old.EndNs < old.StartNs {
		t.Fatalf("old: %+v", old)
	}
	// the later lifetime dies; a line after its end seq is nobody's
	tr.Observe(seqEv(3, 310e9, 1, "am_proc_died", "[0,80,com.bar,900,19]"))
	if p := tr.LookupSeq(80, 311e9, 4); p == nil {
		t.Fatal("grace after end in the same epoch")
	}
	if p := tr.LookupSeq(80, 400e9, 5); p != nil {
		t.Fatalf("after grace: %+v", p)
	}
}

func TestStartSeqRestore(t *testing.T) {
	tr := New()
	tr.Reset("b")
	tr.SetPackages(ParsePackagesU("package:com.foo uid:10001\n"))
	tr.Observe(seqEv(1, 1000e9, 1, "am_proc_start", "[0,50,10001,com.foo,service,{com.foo/com.foo.S}]"))
	tr.Observe(seqEv(2, 1010e9, 1, "am_proc_died", "[0,50,com.foo,900,19]"))
	tr.Observe(seqEv(3, 400e9, 1, "am_proc_start", "[0,51,10001,com.foo:b,service,{com.foo/com.foo.T}]"))
	tr.Observe(seqEv(4, 500e9, 1, "am_proc_start", "[0,52,10001,com.foo:c,service,{com.foo/com.foo.U}]"))
	tr.Observe(seqEv(5, 510e9, 1, "am_proc_died", "[0,52,com.foo:c,900,19]"))
	// lifetimes as the persistence layer holds them
	saved := []Proc{
		*tr.LookupSeq(52, 505e9, 4), *tr.LookupSeq(51, 400e9, 3), *tr.LookupSeq(50, 1005e9, 2),
	}
	// persisted lines carry the seqs; a legacy line loads without them
	b, _ := json.Marshal(saved[2])
	if !strings.Contains(string(b), `"start_seq":1`) || !strings.Contains(string(b), `"end_seq":2`) {
		t.Fatalf("json: %s", b)
	}
	var legacy Proc
	if err := json.Unmarshal([]byte(`{"boot":"b","pid":9,"uid":10001,"name":"com.foo","start_ns":5,"source":"ps"}`), &legacy); err != nil || legacy.StartSeq != 0 {
		t.Fatalf("legacy: %+v %v", legacy, err)
	}
	tr2 := New()
	tr2.Reset("b")
	tr2.Restore([]Proc{saved[2], saved[1], saved[0]}) // file order = creation order is the caller's job
	if p := tr2.LookupSeq(50, 1005e9, 2); p == nil || p.StartSeq != 1 {
		t.Fatalf("restored: %+v", p)
	}
	tr2.Restore([]Proc{legacy})
	if p := tr2.Lookup(9, 6); p == nil {
		t.Fatal("legacy entry resolves by time")
	}
}

// pid reuse across a FORWARD clock jump (device clock set ahead): lines are
// owned by arrival, not by the (now far larger) device times.
func TestLookupSeqAfterForwardJump(t *testing.T) {
	tr := New()
	tr.Reset("b")
	tr.Observe(seqEv(1, 100e9, 1, "am_proc_start", "[0,90,10001,com.foo,service,{com.foo/com.foo.S}]"))
	tr.Observe(seqEv(2, 110e9, 1, "am_proc_died", "[0,90,com.foo,900,19]"))
	tr.ClockJump(3)
	tr.Observe(seqEv(3, 9000e9, 1, "am_proc_start", "[0,90,10002,com.bar,service,{com.bar/com.bar.S}]"))
	for _, c := range []struct {
		seq  uint64
		ns   int64
		want string
	}{
		{2, 105e9, "com.foo"},
		{4, 9001e9, "com.bar"},
		{5, 9500e9, "com.bar"},
		{6, 9e9, "com.bar"}, // stamped with an old-clock time but arrived after the start
	} {
		p := tr.LookupSeq(90, c.ns, c.seq)
		if p == nil || p.Name != c.want {
			t.Errorf("seq %d ns %d: %+v, want %s", c.seq, c.ns, p, c.want)
		}
	}
	old := tr.LookupSeq(90, 105e9, 2)
	if old == nil || old.EndSeq != 2 || old.EndNs != 110e9 {
		t.Fatalf("old: %+v", old)
	}
}
