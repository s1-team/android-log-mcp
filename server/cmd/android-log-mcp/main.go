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
	"github.com/s1crab/android-log-mcp/server/internal/service"
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
  android-log-mcp                              run as MCP server on stdin/stdout (a thin client of the
                                               background service, which it starts when needed)
  android-log-mcp call TOOL [JSON]             debug: call one tool through the service, print the result
      env ANDROID_LOG_MCP_CALL_WAIT=5s: how long to let a service this call started record first
  android-log-mcp report [flags]               pack logs + diagnostics into a redacted zip
                                               (android-log-mcp report -h for flags)
  android-log-mcp root [--json]                print the data root (default: <user cache dir>/android-log-mcp)
  android-log-mcp set-root DIR (--keep-old|--delete-old)
                                               move the data root for every client (android-log-mcp set-root -h)
  android-log-mcp service [status|stop]        the background service (see android-log-mcp service -h)
  android-log-mcp --offline R/store ...         browse a dataset read-only, e.g. an unzipped report (no adb, no writes)
  android-log-mcp compat [--serial S] [--json]  probe a device for the features this tool needs (read-only)

`, mcpserver.Version)
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
	if flag.NArg() > 0 {
		switch flag.Arg(0) {
		case "call", "report", "compat", "root", "service", "set-root":
		default:
			fmt.Fprintf(os.Stderr, "android-log-mcp: unknown command %q\n\n", flag.Arg(0))
			flag.Usage()
			os.Exit(2)
		}
	}
	if flag.NArg() > 0 && flag.Arg(0) == "root" {
		os.Exit(runRoot(flag.Args()[1:]))
	}
	if flag.NArg() > 0 && flag.Arg(0) == "service" {
		if *offline != "" {
			fmt.Fprintln(os.Stderr, "android-log-mcp: --offline cannot be combined with service")
			os.Exit(2)
		}
		os.Exit(runServiceCmd(flag.Args()[1:]))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if flag.NArg() > 0 && flag.Arg(0) == "set-root" {
		if *offline != "" {
			fmt.Fprintln(os.Stderr, "android-log-mcp: --offline cannot be combined with set-root")
			os.Exit(2)
		}
		code := runSetRoot(ctx, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	if flag.NArg() > 0 && flag.Arg(0) == "report" {
		// through the service, or in-process if none runs; never starts one
		code := runReport(ctx, *offline, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	if flag.NArg() > 0 && flag.Arg(0) == "compat" {
		code := runCompat(ctx, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	if *offline == "" {
		os.Exit(runClient(ctx, flag.Args()))
	}
	// --offline: browse a dataset read-only, in this process (no service)
	m := device.NewManager(*offline, store.ConfigFromEnv())
	m.NoADB = true
	m.Version = mcpserver.Version
	m.Start(ctx)
	defer m.Close()
	tools := &mcpserver.Tools{M: m}

	if flag.NArg() > 0 && flag.Arg(0) == "call" {
		if flag.NArg() < 2 {
			flag.Usage()
			os.Exit(2)
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

// runClient runs the MCP stdio server or `call` as a thin client of the
// background service (started on demand; issue #3).
func runClient(ctx context.Context, args []string) int {
	role := "mcp"
	if len(args) > 0 {
		role = args[0]
	}
	cl := service.NewClient(service.ClientOptions{Version: mcpserver.Version, Role: role, Cfg: store.ConfigFromEnv()})
	defer cl.Close()
	if role == "call" {
		if len(args) < 2 {
			flag.Usage()
			return 2
		}
		if _, err := cl.Connect(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "android-log-mcp:", err)
			return 1
		}
		if cl.Started() {
			// a service that just started needs a moment to record
			wait := 5 * time.Second
			if v, err := time.ParseDuration(os.Getenv("ANDROID_LOG_MCP_CALL_WAIT")); err == nil {
				wait = v
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
		raw := json.RawMessage("{}")
		if len(args) > 2 {
			raw = json.RawMessage(args[2])
		}
		text, isErr := cl.Call(ctx, args[1], raw)
		fmt.Println(text)
		if isErr {
			return 1
		}
		return 0
	}
	if len(args) > 0 {
		flag.Usage()
		return 2
	}
	defs, instr := mcpserver.ToolDefs(), mcpserver.Instructions()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	h, err := cl.Connect(cctx)
	cancel()
	if err != nil {
		// keep serving MCP: every tool call reports the problem (and retries)
		log.Printf("cannot reach the background service: %v", err)
	} else {
		defs, instr = h.Tools, h.Instructions
	}
	srv := mcpserver.NewProxyServer(defs, instr, func(ctx context.Context, name string, raw json.RawMessage) (string, bool) {
		return cl.Call(ctx, name, raw)
	})
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Printf("server stopped: %v", err)
	}
	return 0
}
