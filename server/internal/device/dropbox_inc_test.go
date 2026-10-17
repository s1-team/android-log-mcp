package device

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Issue #225 PR3: the DropBox attach carries the incarnation veto (design
// 3.1.1). A DropBox entry has no seq, so its lifetime is the one that owned
// its pid at the entry's device time.

func (r *factRig) dropboxJava(sec int64, pid int, proc string) {
	ts := time.Unix(0, r.ns(sec)).UTC().Format("2006-01-02 15:04:05")
	r.p.ApplyDropbox("Drop box contents\n========================================\n" + ts + " data_app_crash (text, 100 bytes)\nProcess: " + proc +
		"\nPID: " + fmt.Sprint(pid) + "\n\njava.lang.RuntimeException: boom\n\tat " + proc + ".Main.run(Main.java:1)\n")
}

func (r *factRig) amCrashJava(sec int64, pid int, proc string) {
	r.feed(r.rec(sec, "am_crash", 'I', fmt.Sprintf("[0,%d,%s,1,java.lang.RuntimeException,boom,Main.java,1]", pid, proc)))
}

func hasDropbox(fs []string) bool {
	for _, s := range fs {
		if len(s) > 8 && s[:8] == "dropbox:" {
			return true
		}
	}
	return false
}

// The same pid in a second lifetime: the entry of the second crash is not
// the first crash's source, however near, and the far (same pid and process,
// whatever the time) fallback of Java/native does not cross lifetimes either.
func TestDropboxJavaDoesNotCrossLifetimes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
		entry   int64
		reuse   bool // the pid is reused (a start line between)
		want    int
	}{
		{"same lifetime, near", false, 70, false, 1},
		{"same lifetime, an hour later (far fallback)", false, 3600, false, 1},
		{"pid reused, entry of the new lifetime", false, 90, true, 2},
		{"pid reused, entry of the new lifetime, after restart", true, 90, true, 2},
		{"pid reused, entry before the new start still belongs to the old one", true, 70, true, 1},
		{"pid reused, entry an hour later is the new lifetime's", false, 3600, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 100, "com.foo"))
			r.amCrashJava(60, 100, "com.foo")
			if tc.reuse {
				r.feed(r.procStart(75, 100, "com.foo"))
			}
			if tc.restart {
				r.restart()
			}
			r.dropboxJava(tc.entry, 100, "com.foo")
			fs := r.facts()
			if len(fs) != tc.want {
				t.Fatalf("%d facts, want %d: %+v", len(fs), tc.want, fs)
			}
			first := fs[0]
			if got := hasDropbox(first.Sources); got != (tc.want == 1) {
				t.Errorf("first fact has DropBox source = %v (sources %v)", got, first.Sources)
			}
			if tc.want == 2 && !hasDropbox(fs[1].Sources) {
				t.Errorf("the entry of the other lifetime did not become its own fact: %v", fs[1].Sources)
			}
		})
	}
}

// ANR: the interval rule alone would attach the entry of the second
// lifetime to the first ANR (nothing later is anchored); the incarnation
// vetoes it. No lifetime known (no start line): nothing to veto, as before.
func TestDropboxANRIncarnationVeto(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		starts  bool // the process table knows the lifetimes
		reuse   bool
		restart bool
		entry   int64
		want    int
	}{
		{"one lifetime", true, false, false, 90, 1},
		{"reused pid, entry of the new lifetime", true, true, false, 90, 2},
		{"reused pid, entry of the new lifetime, after restart", true, true, true, 90, 2},
		{"reused pid, entry dated before the new start", true, true, true, 70, 1},
		{"unknown lifetimes: no veto", false, false, true, 90, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			if tc.starts {
				r.feed(r.procStart(10, 100, "com.foo"))
			}
			r.feed(r.amANR(60, 100, "com.foo", reasonA))
			r.feed(r.anrBlock(62, 100, "com.foo", reasonA))
			if tc.reuse {
				r.feed(r.procStart(75, 100, "com.foo"))
			}
			if tc.restart {
				r.restart()
			}
			r.dropboxANR(tc.entry, 100, "com.foo", reasonA)
			fs := r.facts()
			if len(fs) != tc.want {
				t.Fatalf("%d facts, want %d", len(fs), tc.want)
			}
			if got := hasDropbox(fs[0].Sources); got != (tc.want == 1) {
				t.Errorf("first fact DropBox = %v (%v)", got, fs[0].Sources)
			}
		})
	}
}

// The logcat ANR arrives after its DropBox-only twin (AdoptDropbox): the
// old 120 s window is replaced by the incarnation and the cap.
func TestAdoptDropboxTwinByIncarnation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reuse bool
		entry int64 // device time of the DropBox entry, the ANR is at 100
		want  int
	}{
		{"near", false, 110, 1},
		{"a slow dump, beyond the old 120 s window", false, 400, 1},
		{"beyond the cap", false, 100 + 601, 2},
		{"the entry belongs to the previous lifetime of the pid", true, 60, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFactRig(t)
			r.feed(r.procStart(10, 100, "com.foo"))
			if tc.reuse {
				r.feed(r.procStart(75, 100, "com.foo"))
			}
			r.dropboxANR(tc.entry, 100, "com.foo", reasonA) // fetched first: a DropBox-only fact
			r.feed(r.amANR(100, 100, "com.foo", reasonA))
			fs := r.facts()
			if len(fs) != tc.want {
				t.Fatalf("%d facts, want %d: %+v", len(fs), tc.want, fs)
			}
			if tc.want == 1 && !(hasSource(fs[0], "events:am_anr") && hasDropbox(fs[0].Sources)) {
				t.Errorf("sources %v", fs[0].Sources)
			}
		})
	}
}

// Race (-race -count=N): the detector path (start line, am_anr, block) and
// the DropBox worker run against each other, so entries arrive before, in
// between and after the logcat sources; every ANR ends as one fact with
// the three evidences and no id is created twice.
func TestDropboxIncarnationConcurrent(t *testing.T) {
	r := newFactRig(t)
	const n = 20
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			pid := 7000 + i
			r.feed(r.procStart(100*int64(i), pid, "com.foo"))
			r.feed(r.amANR(100*int64(i)+50, pid, "com.foo", reasonA))
			r.feed(r.anrBlock(100*int64(i)+52, pid, "com.foo", reasonA))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			r.dropboxANR(100*int64(i)+55, 7000+i, "com.foo", reasonA)
		}
	}()
	wg.Wait()
	fs := r.facts()
	if len(fs) != n {
		t.Fatalf("%d facts, want %d", len(fs), n)
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f.ID] {
			t.Errorf("id %s twice", f.ID)
		}
		seen[f.ID] = true
		if !(hasSource(f, "events:am_anr") && hasSource(f, "logcat:system") && hasDropbox(f.Sources)) || len(f.Ev) != 3 {
			t.Errorf("%s: sources %v, %d ev", f.ID, f.Sources, len(f.Ev))
		}
	}
}
