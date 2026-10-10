package logcat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every line of the real API 29 captures parses (or is a buffer separator).
func TestFixturesParse(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "testdata", "api29", "*.log*"))
	if len(files) == 0 {
		t.Skip("no fixtures")
	}
	loc := time.FixedZone("UTC+0800", 8*3600)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(ln) == "" || strings.HasPrefix(ln, "---------") {
				continue
			}
			r := ParseLine(ln, loc, 2026)
			if r.Divider != "" {
				continue
			}
			if !r.OK || r.TimeNs < 1791572000e9 {
				t.Errorf("%s: unparsed %q", filepath.Base(f), ln)
				continue
			}
			if strings.Contains(r.Msg, `\3`) && strings.Contains(r.Tag, "KeyguardClockSwitch") {
				t.Errorf("printable escape not decoded: %q", r.Msg)
			}
			n++
		}
		if n == 0 {
			t.Errorf("%s: no records", f)
		}
	}
}
