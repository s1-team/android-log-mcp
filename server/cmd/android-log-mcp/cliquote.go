package main

import (
	"runtime"
	"strings"
)

// shellQuote quotes one command-line argument for the shell of the host OS,
// so a `next:` command can be copied as it is: POSIX shells get single
// quotes ('\” for a quote inside); on Windows an argument with a space or
// other special character gets double quotes (a Windows path cannot contain
// one) and a plain path stays bare. Paths on Windows contain spaces and
// commas, so arguments are never joined with a comma.
func shellQuote(s string) string { return shellQuoteFor(runtime.GOOS, s) }

func shellQuoteFor(goos, s string) string {
	if goos == "windows" {
		if s != "" && !strings.ContainsAny(s, " \t&|<>^%(),;=\"") {
			return s
		}
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~=%") {
		return s
	}
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// removeCmd is the command that deletes one directory by hand.
func removeCmd(goos, path string) string {
	if goos == "windows" {
		return "rmdir /s /q " + shellQuoteFor(goos, path)
	}
	return "rm -rf " + shellQuoteFor(goos, path)
}
