package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
)

// runRoot prints the configured data root (read-only).
func runRoot(args []string) int {
	fs := flag.NewFlagSet("root", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: android-log-mcp root [--json]")
		return 2
	}
	cfg, err := layout.ReadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp root:", err)
		return 1
	}
	root, source := cfg.Root, "config"
	if root == "" {
		root, source = layout.DefaultDir(), "default"
	}
	if *asJSON {
		out := map[string]any{"root": root, "source": source, "config": layout.ConfigPath()}
		if cfg.Switch != nil {
			out["pending_switch"] = cfg.Switch
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(root)
	if source == "default" {
		fmt.Fprintf(os.Stderr, "(default; %s does not set a root)\n", layout.ConfigPath())
	}
	if s := cfg.Switch; s != nil {
		fmt.Fprintf(os.Stderr, "pending switch from %s (requested %s, --%s-old): run `android-log-mcp set-root %s --%s-old` to finish it\n",
			s.From, s.RequestedAt.Local().Format(time.RFC3339), s.Old, s.To, s.Old)
	}
	return 0
}

const setRootUsage = `usage: android-log-mcp set-root DIR (--keep-old | --delete-old) [--json] [--timeout 150s]

Moves the data root of every android-log-mcp process of this user to DIR
(stored in %s). Running recorders move on their next maintenance tick
(within ~10 s): they finish the current segment and continue recording in
DIR without a gap; set-root waits for them, then handles the old root.
Logs recorded before the switch are not copied.

Exactly one of:
  --keep-old     leave the old root as it is; it is no longer read or managed
  --delete-old   delete android-log-mcp's structure in the old root (v<N>/, tmp/, run/)
                 and the old root itself if nothing else is left in it

`

// runSetRoot implements `android-log-mcp set-root`.
func runSetRoot(args []string) int {
	fs := flag.NewFlagSet("set-root", flag.ContinueOnError)
	usage := func() {
		fmt.Fprintf(os.Stderr, setRootUsage, layout.ConfigPath())
		fs.PrintDefaults()
	}
	fs.Usage = usage
	keep := fs.Bool("keep-old", false, "keep the old root (no longer read or managed)")
	del := fs.Bool("delete-old", false, "delete android-log-mcp's structure in the old root")
	asJSON := fs.Bool("json", false, "machine-readable result on stdout")
	timeout := fs.Duration("timeout", 150*time.Second, "how long to wait for running recorders to move")
	// flags may follow the directory: set-root DIR --keep-old
	var dir []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		dir = append(dir, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(dir) != 1 || *keep == *del {
		if len(dir) != 1 {
			fmt.Fprintln(os.Stderr, "set-root: exactly one directory is required")
		}
		if *keep == *del {
			fmt.Fprintln(os.Stderr, "set-root: choose exactly one of --keep-old (leave the old root, no longer read or managed) or --delete-old (delete android-log-mcp's data in the old root)")
		}
		usage()
		return 2
	}
	opt := layout.SetRootOptions{Target: dir[0], KeepOld: *keep, DeleteOld: *del, Timeout: *timeout}
	if !*asJSON {
		opt.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "set-root: "+f+"\n", a...) }
		fmt.Fprintln(os.Stderr, "set-root: waiting for running recorders to move (up to", *timeout, ")...")
	}
	res, err := layout.SetRoot(opt)
	if *asJSON {
		out := struct {
			layout.SetRootResult
			Error string `json:"error,omitempty"`
		}{SetRootResult: res}
		if err != nil {
			out.Error = err.Error()
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		printSetRoot(res, err)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, layout.ErrSwitchTimeout):
		return 3
	case res.OldRoot == "":
		return 2 // validation
	default:
		return 1
	}
}

func printSetRoot(r layout.SetRootResult, err error) {
	if err != nil && r.Outcome == "" {
		fmt.Fprintln(os.Stderr, "set-root: error:", err)
		return
	}
	fmt.Printf("data root: %s (was %s)\n", r.NewRoot, r.OldRoot)
	for _, m := range r.Moved {
		fmt.Printf("  recorder moved: %s\n", m)
	}
	if len(r.Moved) == 0 && r.Outcome == "switched" {
		fmt.Println("  no recorder was running: switched immediately")
	}
	if r.Outcome == "timeout" {
		for _, w := range r.Waiting {
			fmt.Printf("  NOT moved after %s: %s\n", r.Waited, w)
		}
		fmt.Printf("timed out: new processes already use %s, but the recorder(s) above did not move\n", r.NewRoot)
		fmt.Printf("(an older version that does not know set-root?). The old root %s was not touched.\n", r.OldRoot)
		fmt.Printf("Restart those MCP clients, then run `android-log-mcp set-root %s --%s-old` again to finish.\n", r.NewRoot, r.Old)
		return
	}
	switch r.Old {
	case "keep":
		fmt.Printf("old root kept: %s — it is no longer read or managed by android-log-mcp; delete it yourself when no longer needed\n", r.OldRoot)
	case "delete":
		for _, p := range r.Removed {
			fmt.Printf("  deleted %s\n", p)
		}
		if r.OldRootRemoved {
			fmt.Printf("old root removed: %s\n", r.OldRoot)
		} else {
			fmt.Printf("old root %s kept, it still contains (not ours, or shared):\n", r.OldRoot)
			for _, p := range r.Remained {
				fmt.Printf("  %s\n", p)
			}
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintln(os.Stderr, "set-root: warning:", e)
	}
}
