// Command android-log-mcp is an MCP stdio server exposing Android logcat.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// version is set by the release build (-ldflags "-X main.version=x.y.z").
var version string

func main() {
	if version != "" {
		mcpserver.Version = version
	}
	version := flag.Bool("version", false, "print version and exit")
	offline := flag.String("offline", "", "open the dataset in `DIR` read-only (e.g. an unzipped report's store/): no adb, never records, prunes or creates files")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `android-log-mcp %s — MCP stdio server for Android logcat

usage:
  android-log-mcp                              run as MCP server on stdin/stdout
  android-log-mcp call TOOL [JSON]             debug: record for a few seconds, call one tool, print result
      env ANDROID_LOG_MCP_CALL_WAIT=5s controls how long to record before the call
  android-log-mcp report [flags]               pack logs + diagnostics into a redacted zip
                                               (android-log-mcp report -h for flags)
  android-log-mcp root [--json]                print the data root (from %s)
  android-log-mcp set-root DIR (--keep-old | --delete-old) [--json] [--timeout 150s]
                                               move the data root for all processes, live
  android-log-mcp --offline R/store ...         browse a dataset read-only, e.g. an unzipped report (no adb, no writes)
  android-log-mcp compat [--serial S] [--json]  probe a device for the features this tool needs (read-only)

`, mcpserver.Version, layout.ConfigPath())
		flag.PrintDefaults()
	}
	for _, a := range os.Args[1:] {
		if a == "--" {
			break
		}
		if a == "-home" || a == "--home" || strings.HasPrefix(a, "-home=") || strings.HasPrefix(a, "--home=") {
			fmt.Fprintln(os.Stderr, "android-log-mcp: error:", layout.ErrHomeRemoved)
			os.Exit(2)
		}
	}
	flag.Parse()
	if *version {
		fmt.Println(mcpserver.Version)
		return
	}
	log.SetOutput(os.Stderr) // stdout belongs to the MCP protocol
	log.SetPrefix("android-log-mcp: ")
	if err := layout.CheckRemovedHome(); err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp: error:", err)
		os.Exit(2)
	}
	if flag.NArg() > 0 && (flag.Arg(0) == "root" || flag.Arg(0) == "set-root") {
		if *offline != "" {
			fmt.Fprintln(os.Stderr, "android-log-mcp: --offline is not used with", flag.Arg(0))
			os.Exit(2)
		}
		if flag.Arg(0) == "root" {
			os.Exit(runRoot(flag.Args()[1:]))
		}
		os.Exit(runSetRoot(flag.Args()[1:]))
	}
	base := *offline
	if base == "" {
		var err error
		if base, err = layout.ConfiguredRoot(); err != nil {
			fmt.Fprintln(os.Stderr, "android-log-mcp: error:", err)
			os.Exit(2)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if flag.NArg() > 0 && flag.Arg(0) == "report" {
		// reads the data directory only; must not start a recorder
		code := runReport(ctx, base, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	if flag.NArg() > 0 && flag.Arg(0) == "compat" {
		code := runCompat(ctx, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	m := device.NewManager(base, store.ConfigFromEnv())
	m.NoADB = *offline != ""
	m.FollowConfig = !m.NoADB
	m.Version = mcpserver.Version
	m.Start(ctx)
	defer m.Close()
	tools := &mcpserver.Tools{M: m}

	if flag.NArg() > 0 && flag.Arg(0) == "call" {
		if flag.NArg() < 2 {
			flag.Usage()
			os.Exit(2)
		}
		wait := 5 * time.Second
		if *offline != "" {
			wait = 0
		}
		if v, err := time.ParseDuration(os.Getenv("ANDROID_LOG_MCP_CALL_WAIT")); err == nil {
			wait = v
		}
		m.WaitReady(5 * time.Second)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		raw := json.RawMessage("{}")
		if flag.NArg() > 2 {
			raw = json.RawMessage(flag.Arg(2))
		}
		text, isErr := tools.Call(ctx, flag.Arg(1), raw)
		fmt.Println(text)
		if isErr {
			os.Exit(1)
		}
		return
	}
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	srv := mcpserver.NewServer(tools)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Printf("server stopped: %v", err)
	}
}
