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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// version is set by the release build (-ldflags "-X main.version=x.y.z").
var version string

func main() {
	if version != "" {
		mcpserver.Version = version
	}
	home := flag.String("home", "", "data directory (default $ANDROID_LOG_MCP_HOME or <user cache dir>/android-log-mcp)")
	version := flag.Bool("version", false, "print version and exit")
	offline := flag.Bool("offline", false, "do not use adb: only browse recorded data (e.g. an unzipped report's store/ directory)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `android-log-mcp %s — MCP stdio server for Android logcat

usage:
  android-log-mcp [--home DIR]                 run as MCP server on stdin/stdout
  android-log-mcp [--home DIR] call TOOL [JSON] debug: record for a few seconds, call one tool, print result
      env ANDROID_LOG_MCP_CALL_WAIT=5s controls how long to record before the call
  android-log-mcp [--home DIR] report [flags]   pack logs + diagnostics into a redacted zip
                                               (android-log-mcp report -h for flags)
  android-log-mcp --home R/store --offline ...  browse an unzipped report (no adb, no recording)

`, mcpserver.Version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *version {
		fmt.Println(mcpserver.Version)
		return
	}
	log.SetOutput(os.Stderr) // stdout belongs to the MCP protocol
	log.SetPrefix("android-log-mcp: ")
	base := *home
	if base == "" {
		base = device.DefaultBase()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if flag.NArg() > 0 && flag.Arg(0) == "report" {
		// reads the data directory only; must not start a recorder
		code := runReport(ctx, *home, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	m := device.NewManager(base, store.Config{})
	m.NoADB = *offline
	m.Start(ctx)
	defer m.Close()
	tools := &mcpserver.Tools{M: m}

	if flag.NArg() > 0 && flag.Arg(0) == "call" {
		if flag.NArg() < 2 {
			flag.Usage()
			os.Exit(2)
		}
		wait := 5 * time.Second
		if *offline {
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
