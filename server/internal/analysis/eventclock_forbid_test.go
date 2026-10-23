package analysis

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Forbidden local clock formulas after R1 Phase B (#258): analysis must use
// EventClock (ShiftCompare / Continuous / SameEpoch / SumDeltas) instead of
// private adj/cum/dropboxDelta/shiftAfter reimplementations.
// Mark a rare exception with "// eventclock:allow" on the same line.

var (
	forbidJumpNs  = regexp.MustCompile(`\bjumpNs\b`)
	forbidShiftFn = regexp.MustCompile(`func\s+shiftAfter\s*\(`)
	forbidCumMake = regexp.MustCompile(`cum\s*:=\s*make\(\[\]int64`)
	// Two Record/Crash TimeNs fields subtracted (cross-event device time).
	forbidTimeNsSub = regexp.MustCompile(`\w+\.TimeNs\s*-\s*\w+\.TimeNs`)
	// Classic dropboxDelta raw/shift formula outside clock package.
	forbidShiftFormula = regexp.MustCompile(`abs\(\w+[Nn]s\s*[+\-]\s*\w+.*-\s*\w+[Nn]s\)|abs\(\w+[Nn]s\s*-\s*\w+[Nn]s\)`)
)

func TestNoLocalClockFormulas(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// This file's own patterns live in the test binary, not scanned sources.
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for i, line := range strings.Split(string(src), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") || strings.Contains(line, "eventclock:allow") {
				continue
			}
			n := i + 1
			check := func(re *regexp.Regexp, why string) {
				if re.MatchString(line) {
					t.Errorf("%s:%d: %s (use EventClock; or mark // eventclock:allow):\n\t%s", name, n, why, trim)
				}
			}
			check(forbidJumpNs, "private jumpNs accumulator")
			check(forbidShiftFn, "local shiftAfter; use EventClock.ShiftCompare")
			check(forbidCumMake, "local cum[] continuous axis; use EventClock.Continuous")
			check(forbidTimeNsSub, "raw TimeNs−TimeNs across events")
			check(forbidShiftFormula, "local abs(ns−ns) / shift distance; use EventClock.ShiftCompare")
		}
	}
	if files < 5 {
		t.Fatalf("scanned only %d analysis sources; wrong dir %s?", files, dir)
	}
}
