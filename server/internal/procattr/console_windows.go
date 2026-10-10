//go:build windows

package procattr

import "golang.org/x/sys/windows"

var getConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// ConsoleWindow reports this process's console window, if any, and whether
// it is visible. Used by tests that check children open no window.
func ConsoleWindow() (hwnd uintptr, visible bool) {
	h, _, _ := getConsoleWindow.Call()
	if h == 0 {
		return 0, false
	}
	return h, windows.IsWindowVisible(windows.HWND(h))
}
