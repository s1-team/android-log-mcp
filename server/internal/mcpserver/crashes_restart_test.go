package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/device"
)

// Issue #225 (design 1.3, test plan 7, the deterministic half of emulator
// scenarios E2/E3): the service process restarts between two sources of one
// crash (new Detector, new process table from procs.jsonl, same files). The
// crash list must equal the one of the uninterrupted run, and an ID printed
// before the restart must still name the crash.

func (e *replayEnv) restartService() {
	loc, _ := device.ResolveZone("Asia/Kuching", "+0800", fxNow())
	e.pipe = device.NewPipeline(e.st, e.boot, loc)
}

func TestRestartBetweenSourcesEqualsUninterruptedRun(t *testing.T) {
	t.Parallel()
	dropboxes := map[string][]string{
		"java_crash.logcat.txt":    {"dropbox-data_app_crash.txt"},
		"native_crash.logcat.txt":  {"dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt"},
		"anr_broadcast.logcat.txt": {"dropbox-data_app_anr.txt"},
	}
	for _, tc := range []struct {
		file  string
		split float64 // epoch second of the first line after the restart
		what  string
	}{
		{"java_crash.logcat.txt", 1791572492.43, "between FATAL EXCEPTION and am_crash"},
		{"native_crash.logcat.txt", 1791572565, "between the tombstone and the am_crash Native event"},
		{"native_crash.logcat.txt", 1791572580, "after the am_crash Native event"},
		{"anr_broadcast.logcat.txt", 1791572760, "between am_anr and the ANR block"},
		{"anr_broadcast.logcat.txt", 1791572800, "after the ANR block"},
	} {
		t.Run(fmt.Sprintf("%s %s", strings.TrimSuffix(tc.file, ".logcat.txt"), tc.what), func(t *testing.T) {
			t.Parallel()
			text := fixtureFile(t, tc.file)
			ref := newReplay(t, fxBoot1)
			ref.replay(text)
			for _, d := range dropboxes[tc.file] {
				ref.pipe.ApplyDropbox(fixtureFile(t, d))
			}
			want := pkgCrashes(ref)

			e := newReplay(t, fxBoot1)
			a, b := splitAt(text, tc.split)
			if b == "" {
				t.Fatalf("split %v is past the capture", tc.split)
			}
			e.replay(a)
			e.restartService()
			// the capture's buffer header precedes the cut, a restarted
			// reader starts a new logcat with its own
			if i := strings.LastIndex(a, "--------- beginning of "); i >= 0 {
				b = a[i:i+strings.IndexByte(a[i:], '\n')+1] + b
			}
			e.replay(b)
			e.restartService() // and again before the DropBox fetch
			for _, d := range dropboxes[tc.file] {
				e.pipe.ApplyDropbox(fixtureFile(t, d))
			}
			got := pkgCrashes(e)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("after a restart:\n%s\nuninterrupted:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if len(got) == 0 {
				t.Fatal("no crash")
			}
		})
	}
}
