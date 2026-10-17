package analysis

// Issue #176: DropBox entries of ANRs are matched one-to-one. Two ANRs of
// one pid 60 s apart: each entry attaches to its own ANR whatever the
// order the entries arrive in, and an entry never replaces the traces of
// an ANR that already has one.

import (
	"strings"
	"testing"
)

func anrEntry(headerNs, bodyNs int64, marker string) DropboxEntry {
	return DropboxEntry{Tag: "data_app_anr", TimeNs: headerNs, BodyTimeNs: bodyNs,
		Body: "Process: com.foo\nPID: 4321\nSubject: x\n\n----- pid 4321 at x -----\nCmd line: com.foo\n" + marker + "\n"}
}

// twoANRs returns two complete ANRs of pid 4321 at 1000 s and 1060 s.
func twoANRs(t *testing.T) []*Crash {
	r := newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1005, 4321)
	r.amANR(1060, 4321)
	r.anrBlock(1065, 4321)
	g := anrs(r)
	if len(g) != 2 {
		t.Fatalf("setup: %d ANRs, want 2", len(g))
	}
	return g
}

// attach matches e against list and stores the result back like the
// pipeline does (the matched copy replaces its crash).
func attach(t *testing.T, list []*Crash, e DropboxEntry) *Crash {
	t.Helper()
	c, isNew := MatchDropboxBoot(list, e, "", nil)
	if c == nil || isNew {
		return nil
	}
	for i, x := range list {
		if x.TimeNs == c.TimeNs {
			list[i] = c
		}
	}
	return c
}

func TestDropboxANROneToOne(t *testing.T) {
	const s = int64(1e9)
	for _, tc := range []struct {
		name       string
		body       bool // entries carry a body time
		reverse    bool // ANR2's entry arrives first
		wantTraces [2]string
	}{
		{"header time, in order", false, false, [2]string{"E1", "E2"}},
		{"header time, reversed", false, true, [2]string{"E1", "E2"}},
		{"body time, in order", true, false, [2]string{"E1", "E2"}},
		{"body time, reversed", true, true, [2]string{"E1", "E2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := twoANRs(t)
			t1, t2 := g[0].TimeNs, g[1].TimeNs
			var b1, b2 int64
			if tc.body {
				b1, b2 = t1+3*s, t2+3*s
			}
			e1 := anrEntry(t1+40*s, b1, "E1") // ANR1's entry, written 40 s after it
			e2 := anrEntry(t2+40*s, b2, "E2")
			order := []DropboxEntry{e1, e2}
			if tc.reverse {
				order = []DropboxEntry{e2, e1}
			}
			for _, e := range order {
				if attach(t, g, e) == nil {
					t.Fatalf("entry %q not attached", e.Body[len(e.Body)-3:])
				}
			}
			for i, w := range tc.wantTraces {
				if !strings.Contains(g[i].Traces, w) {
					t.Errorf("ANR%d traces = %q, want entry %s", i+1, g[i].Traces, w)
				}
			}
		})
	}
}

// An ANR that has its entry is never given another one, and the traces it
// has stay.
func TestDropboxANRNeverOverwritten(t *testing.T) {
	const s = int64(1e9)
	g := twoANRs(t)
	if attach(t, g, anrEntry(g[0].TimeNs+10*s, 0, "OWN")) == nil {
		t.Fatal("own entry not attached")
	}
	// a second, worse entry (30 s after ANR1, header time only) must not
	// replace ANR1's traces; ANR2 is later than the entry, so it does not
	// take it either
	c, isNew := MatchDropboxBoot(g, anrEntry(g[0].TimeNs+30*s, 0, "WORSE"), "", nil)
	if isNew {
		t.Errorf("a second entry of a paired ANR made a new crash")
	}
	if c != nil && c.TimeNs == g[0].TimeNs {
		t.Errorf("ANR1 got a second entry")
	}
	if !strings.Contains(g[0].Traces, "OWN") || strings.Contains(g[0].Traces, "WORSE") {
		t.Errorf("ANR1 traces = %q", g[0].Traces)
	}
	// a matched copy that already holds traces keeps them
	full := *g[1]
	full.Traces = "KEEP"
	full.Sources = nil
	if c, _ := MatchDropboxBoot([]*Crash{&full}, anrEntry(full.TimeNs+10*s, 0, "NEW"), "", nil); c == nil || c.Traces != "KEEP" {
		t.Errorf("existing traces replaced: %+v", c)
	}
}

// The ANR logged after the entry is not its source: with a single ANR
// that follows the entry, it is still matched (nothing better exists).
func TestDropboxANRLaterOnlyStillMatches(t *testing.T) {
	r := newJumpRig(t)
	r.amANR(1000, 4321)
	r.anrBlock(1005, 4321)
	g := anrs(r)
	if c, _ := MatchDropboxBoot(g, anrEntry(g[0].TimeNs-20*int64(1e9), 0, "E"), "", nil); c == nil {
		t.Errorf("entry dated before the only ANR was dropped")
	}
}
