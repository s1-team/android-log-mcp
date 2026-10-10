package procs

import (
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
	ps := tr.ForPackage("com.foo")
	if len(ps) != 2 {
		t.Fatalf("for package: %+v", ps)
	}
	var dead *Proc
	for i := range ps {
		if ps[i].PID == 4321 {
			dead = &ps[i]
		}
	}
	if dead == nil || dead.Alive() || !dead.Crashed || dead.EndNs != 20e9+5e6 {
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
