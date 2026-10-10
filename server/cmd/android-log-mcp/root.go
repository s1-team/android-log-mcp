package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

// runRoot prints the data root (read-only). It is fixed to
// <user cache dir>/android-log-mcp; a configurable root (set-root) is
// planned for the background service (#3, #10).
func runRoot(args []string) int {
	fs := flag.NewFlagSet("root", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: android-log-mcp root [--json]")
		return 2
	}
	root := layout.DefaultDir()
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]string{"root": root, "owners": layout.DefaultOwnersDir()}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(root)
	return 0
}
