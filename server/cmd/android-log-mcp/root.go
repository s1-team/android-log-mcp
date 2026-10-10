package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/service"
)

// runRoot prints the data root (read-only): the root the service records
// into (config.json, written by set-root; default <user cache dir>/android-log-mcp).
func runRoot(args []string) int {
	fs := flag.NewFlagSet("root", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: android-log-mcp root [--json]")
		return 2
	}
	ctrl := service.DefaultControl()
	root, err := service.ConfiguredRoot(ctrl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp root:", err)
		return 1
	}
	if *asJSON {
		out := map[string]any{"root": root, "default_root": ctrl.Dir, "control": ctrl.Dir, "config": ctrl.ConfigPath(), "socket": ctrl.Socket()}
		if st := ctrl.ReadState(); st != nil && service.Running(ctrl) {
			out["service"] = map[string]any{"pid": st.PID, "version": st.Version, "proto": st.Proto, "root": st.Root}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(root)
	return 0
}
