package fakeadb

import "testing"

func zombie(pid int) bool {
	st, _, ok := procStat(pid)
	return ok && st == 'Z'
}

func TestParseStat(t *testing.T) {
	for _, c := range []struct {
		in    string
		state byte
		flags uint64
		ok    bool
	}{
		{"10 (adbhang) Z 53 1 1 0 -1 4228364 3992 0", 'Z', 4228364, true},
		{"11 (a b) c)) R 53 1 1 0 -1 4195596 1", 'R', 4195596, true},
		{"12 (x) D 1 1 1 0 -1 4194560", 'D', 4194560, true},
		{"13 (x) S 1", 0, 0, false},
		{"garbage", 0, 0, false},
	} {
		st, fl, ok := parseStat(c.in)
		if st != c.state || fl != c.flags || ok != c.ok {
			t.Errorf("parseStat(%q) = %c %d %v", c.in, st, fl, ok)
		}
	}
	// D (uninterruptible sleep) stays alive; only R/S/D... with PF_EXITING is exiting
	if _, fl, _ := parseStat("12 (x) D 1 1 1 0 -1 4194560"); fl&pfExiting != 0 {
		t.Error("D sample must not be exiting")
	}
	if _, fl, _ := parseStat("11 (a b) c)) R 53 1 1 0 -1 4195596 1"); fl&pfExiting == 0 {
		t.Error("R sample from #61 must be exiting")
	}
}
