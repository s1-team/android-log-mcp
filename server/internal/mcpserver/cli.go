package mcpserver

// The CLI commands (call, record, report, service status, set-root, ...)
// print the same key=value grammar as the tools (issue #85), so one parser
// reads both. These are the only exports of format.go's helpers.

// KV is "word k=v k=v" (values with a space are quoted).
func KV(word string, kv ...any) string { return hdr(word, kv...) }

// CLIError is "error code=<code> k=v ... next: <executable step>" without a
// newline. next is a command line (or param=value), never prose.
func CLIError(code, next string, kv ...any) string {
	return hdr("error", append([]any{"code", code}, kv...)...) + nextClause(next)
}

// CLIErrorAlts is CLIError with several alternative commands. A CLI `next:`
// line holds exactly one command (never `a | b`, `;` or `&&`: pasted into a
// shell, alternatives would all run); the alternatives are further lines that
// each start with `next:`. A single alternative stays on the error line.
func CLIErrorAlts(code string, next []string, kv ...any) string {
	if len(next) <= 1 {
		n := ""
		if len(next) == 1 {
			n = next[0]
		}
		return CLIError(code, n, kv...)
	}
	out := hdr("error", append([]any{"code", code}, kv...)...)
	for _, n := range next {
		out += "\nnext: " + n
	}
	return out
}
