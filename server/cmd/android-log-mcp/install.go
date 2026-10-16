package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

// runInstall copies this binary to a fixed path that MCP clients can run
// directly (recommended on Windows: no npx.cmd / cmd.exe / node chain).
func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: android-log-mcp install [--json]

Copies this android-log-mcp binary to a fixed path and prints it:
  %s
Configure your MCP host to run that path directly (no args) instead of
npx. Run it again (e.g. npx -y android-log-mcp@latest install) to update;
a copy that is running is replaced safely.
`, service.DefaultControl().StableExe())
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	p, updated, err := service.InstallStable(service.DefaultControl())
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp install:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"path": p, "version": mcpserver.Version, "updated": updated}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(p)
	state := "installed"
	if !updated {
		state = "already up to date"
	}
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"android-log": map[string]any{"command": p, "args": []string{}}}})
	fmt.Fprintf(os.Stderr, "android-log-mcp %s %s at %s\nMCP host config (run the binary directly):\n  %s\n", mcpserver.Version, state, p, cfg)
	return 0
}
