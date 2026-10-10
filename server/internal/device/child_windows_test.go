//go:build windows

package device

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

// Every adb child must start without a console window; start-server also in
// its own process group (the adb server is never part of ours).
func TestADBChildrenHaveNoWindow(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		hide  bool
		flags uint32
	}{
		"adb":          {adbCmd(ctx, "adb.exe", "devices").SysProcAttr.HideWindow, adbCmd(ctx, "adb.exe", "devices").SysProcAttr.CreationFlags},
		"start-server": {startServerCmd(ctx, "adb.exe").SysProcAttr.HideWindow, startServerCmd(ctx, "adb.exe").SysProcAttr.CreationFlags},
	} {
		if !c.hide || c.flags&windows.CREATE_NO_WINDOW == 0 {
			t.Errorf("%s: HideWindow %v flags %#x, want CREATE_NO_WINDOW", name, c.hide, c.flags)
		}
	}
	if f := startServerCmd(ctx, "adb.exe").SysProcAttr.CreationFlags; f&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Errorf("start-server flags %#x, want CREATE_NEW_PROCESS_GROUP", f)
	}
}
