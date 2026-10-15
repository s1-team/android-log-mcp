package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
)

// parseFlags parses args for a subcommand. A usage mistake prints one
// `error code=usage ... next: <example>` line (exit 2), never the Go flag
// error plus the whole usage block; only -h prints the full usage (exit 0).
// fs.Usage must print the full usage.
func parseFlags(fs *flag.FlagSet, args []string, example string) (code int, ok bool) {
	full := fs.Usage
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	if err == nil {
		fs.Usage = full
		return 0, true
	}
	if errors.Is(err, flag.ErrHelp) {
		full()
		return 0, false
	}
	fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", example, "msg", err.Error()))
	return 2, false
}
