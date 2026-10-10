// Package procattr holds the process attributes every child process of
// android-log-mcp needs.
//
// On Windows a console program (adb.exe, our own service binary) started by
// a process that has no console of its own gets a new, visible console
// window. MCP clients such as Codex or ChatGPT start us without a visible
// console, so every adb call used to flash a window. Hide marks a command to
// run without a console window (CREATE_NO_WINDOW, SW_HIDE); its own console
// children then inherit that hidden console instead of creating one.
//
// Every exec.Command / exec.CommandContext in non-test code must be wrapped
// in Hide; TestEveryChildIsHidden enforces it
// (a line marked procattr:allow is exempt).
package procattr
