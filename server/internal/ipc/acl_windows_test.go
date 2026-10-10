//go:build windows

package ipc

import (
	"strings"
	"testing"
)

func checkUserOnlyACL(t *testing.T, dir string) {
	sddl, err := DirSDDL(dir)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := CurrentUserSID()
	if !strings.Contains(sddl, "D:P") || !strings.Contains(sddl, sid) {
		t.Fatalf("dacl %s (user %s)", sddl, sid)
	}
	for _, broad := range []string{";WD)", ";BU)", ";AU)", ";IU)"} {
		if strings.Contains(sddl, broad) {
			t.Fatalf("dacl grants %s: %s", broad, sddl)
		}
	}
}
