package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if dir != "" {
			fmt.Fprintf(os.Stderr, "set-root: unexpected argument %q\n\n", fs.Arg(0))
			fs.Usage()
			return 2
		}
		dir, rest = fs.Arg(0), fs.Args()[1:]
	}
	if dir == "" || *keep == *del {
		if dir != "" {
			fmt.Fprintln(os.Stderr, "set-root: exactly one of --keep-old or --delete-old is required")
			fmt.Fprintln(os.Stderr)
		}
		fs.Usage()
		return 2
	}
	res, err := service.SetRoot(ctx, service.DefaultControl(), mcpserver.Version, service.SetRootArgs{Target: dir, DeleteOld: *del, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp set-root:", err)
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
	by := "by the running service (pid " + fmt.Sprint(res.ServicePID) + ")"
	if res.ServicePID == 0 {
		by = "(no service was running)"
	}
	fmt.Printf("data root: %s (was %s), switched %s in %s\n", res.NewRoot, res.OldRoot, by, res.Took)
	if len(res.Devices) > 0 {
		fmt.Printf("recording continues there for: %v (history before the switch stays in the old root)\n", res.Devices)
	}
	switch {
	case res.Old == "keep":
		fmt.Printf("old root kept: %s\n", res.OldRoot)
	case len(res.Remained) == 0:
		what := "removed"
		if !res.OldRootRemoved {
			what = "android-log-mcp's directories removed (other files, or the control files, remain)"
		}
		fmt.Printf("old root %s: %s\n", res.OldRoot, what)
	default:
		fmt.Printf("old root %s: could not remove %v (%v); the service retries when it next starts, or remove them by hand\n", res.OldRoot, res.Remained, res.Errors)
	}
	return code
}
