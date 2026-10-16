package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

const setRootUsage = `usage: android-log-mcp set-root DIR (--keep-old | --delete-old) [--timeout 60s] [--json]

Moves the data root (where the background service records) to the absolute
directory DIR, for every client. The running service stops recording,
carries each device's state over (the seqs, the resume point; history stays
in the old root), commits the new root to config.json and resumes recording
there; any failure before the commit is rolled back. With no service running
the switch is done by this command (it does not start a service).

  --keep-old     leave the old root as it is (its history is no longer queried)
  --delete-old   remove android-log-mcp's directories from the old root
                 (v<N>/, tmp/, bin/, log/), and the old root if then empty

DIR must be new or empty, or an earlier android-log-mcp root, and must not be
inside or a parent of the current root. Exit status: 0 done, 1 failed
(nothing changed), 2 usage, 3 switched but some of the old root could not be
removed (retried when the service next starts).
`

func runSetRoot(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("set-root", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, setRootUsage) }
	keep := fs.Bool("keep-old", false, "keep the old root")
	del := fs.Bool("delete-old", false, "delete android-log-mcp's directories from the old root")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for another set-root or running tool calls")
	asJSON := fs.Bool("json", false, "machine-readable output")
	// accept DIR before or after the flags
	var dir string
	rest := args
	for {
		if code, ok := parseFlags(fs, rest, "android-log-mcp set-root --help"); !ok {
			return code
		}
		if fs.NArg() == 0 {
			break
		}
		if dir != "" {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp set-root "+shellQuote(dir)+" --keep-old", "value", fs.Arg(0), "msg", "unexpected_argument"))
			return 2
		}
		dir, rest = fs.Arg(0), fs.Args()[1:]
	}
	if dir == "" || *keep == *del {
		msg, next := "missing_dir", "android-log-mcp root" // prints the current root; pass the new one to set-root
		if dir != "" {
			msg = "exactly_one_of_keep_old_delete_old"
			next = "android-log-mcp set-root " + shellQuote(dir) + " --keep-old | android-log-mcp set-root " + shellQuote(dir) + " --delete-old"
		}
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", next, "msg", msg))
		return 2
	}
	res, err := service.SetRoot(ctx, service.DefaultControl(), mcpserver.Version, service.SetRootArgs{Target: dir, DeleteOld: *del, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("set_root_failed", "android-log-mcp root", "msg", err.Error()))
		return 1
	}
	code := 0
	if len(res.Remained) > 0 {
		code = 3
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return code
	}
	fmt.Println(mcpserver.KV("ok set-root", "root", res.NewRoot, "old_root", res.OldRoot, "service_pid", res.ServicePID, "took_s", tookSeconds(res.Took), "old", oldState(res)))
	if len(res.Devices) > 0 {
		fmt.Println(mcpserver.KV("note", "code", "recording_continues", "devices", strings.Join(res.Devices, ",")))
	}
	if len(res.Remained) > 0 {
		fmt.Println(mcpserver.KV("warning", "code", "old_root_remains", "count", len(res.Remained), "msg", fmt.Sprint(res.Errors)))
		for _, p := range res.Remained { // one line per path: a path can contain spaces and commas
			fmt.Println(mcpserver.KV("remains", "path", p) + " next: " + removeCmd(runtime.GOOS, p))
		}
	}
	return code
}

func oldState(res *service.SetRootResult) string {
	switch {
	case res.Old == "keep":
		return "kept"
	case len(res.Remained) > 0:
		return "partly_removed"
	case res.OldRootRemoved:
		return "removed"
	}
	return "dirs_removed"
}

func tookSeconds(s string) float64 {
	d, _ := time.ParseDuration(s)
	return float64(int(d.Seconds()*10)) / 10
}
