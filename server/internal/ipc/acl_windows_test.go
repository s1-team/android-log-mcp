//go:build windows

package ipc

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

var aceRe = regexp.MustCompile(`\(([^;]*);([^;]*);([^;]*);([^;]*);([^;]*);([^)]*)\)`)

// checkUserOnlyACL: a protected DACL whose allow entries are only the
// current user and SYSTEM. SDDL may print a SID as an alias (e.g. "LA" for
// the built-in Administrator, which CI runners use), so trustees are
// resolved before comparing.
func checkUserOnlyACL(t *testing.T, dir string) {
	sddl, err := DirSDDL(dir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sddl, "D:P") {
		t.Fatalf("dacl not protected: %s", sddl)
	}
	dacl := sddl[strings.Index(sddl, "D:"):]
	sawUser := false
	for _, m := range aceRe.FindAllStringSubmatch(dacl, -1) {
		sid, err := windows.StringToSid(m[6])
		if err != nil {
			t.Fatalf("ace trustee %q: %v (%s)", m[6], err, sddl)
		}
		switch s := sid.String(); {
		case s == user:
			sawUser = true
		case s == "S-1-5-18": // SYSTEM
		default:
			if m[1] == "A" {
				t.Fatalf("dacl allows %s (%s): %s", m[6], s, sddl)
			}
		}
	}
	if !sawUser {
		t.Fatalf("dacl lacks the current user %s: %s", user, sddl)
	}
}
