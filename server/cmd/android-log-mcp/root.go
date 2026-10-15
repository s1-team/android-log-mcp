package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

// runRoot prints the data root (read-only): the root the service records
// into (config.json, written by set-root; default <user cache dir>/android-log-mcp).
func runRoot(args []string) int {
	fs := flag.NewFlagSet("root", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp root", "msg", "no_arguments_besides_json"))
		return 2
	}
	ctrl := service.DefaultControl()
	root, err := service.ConfiguredRoot(ctrl)
	if err != nil {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("config_unusable", "android-log-mcp service status", "msg", err.Error()))
		return 1
	}
	if *asJSON {
		out := map[string]any{"root": root, "default_root": ctrl.Dir, "control": ctrl.Dir, "config": ctrl.ConfigPath(), "socket": ctrl.Socket()}
		if _, err := os.Stat(ctrl.StableExe()); err == nil {
			out["exe"] = ctrl.StableExe() // android-log-mcp install
		}
		if st := ctrl.ReadState(); st != nil && service.Running(ctrl) {
			svc := map[string]any{"pid": st.PID, "version": st.Version, "proto": st.Proto, "root": st.Root}
			// what it records (issue #11); asking starts nothing
			if c, err := service.Dial(context.Background(), ctrl, mcpserver.Version, "cli"); err == nil {
				var info service.Info
				if c.Do(context.Background(), "status", nil, &info) == nil && info.Recording != "" {
					svc["recording"] = info.Recording
					if len(info.Devices) > 0 {
						svc["devices"] = info.Devices
					}
					if len(info.Pins) > 0 {
						svc["pins"] = info.Pins
					}
				}
				c.Close()
			}
			out["service"] = svc
		} else {
			out["service"] = map[string]any{"running": false, "recording": "idle (no service running)"}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(root)
	return 0
}
