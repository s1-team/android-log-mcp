// Command android-log-mcp is an MCP stdio server exposing Android logcat.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
  android-log-mcp record [--device X]... --for 2h | --stop | [--json]
                                               keep recording a device until a deadline, stop it, or show
                                               what is recorded (android-log-mcp record -h)
  android-log-mcp report [flags]               pack logs + diagnostics into a redacted zip
                                               (android-log-mcp report -h for flags)
  android-log-mcp root [--json]                print the data root (default: <user cache dir>/android-log-mcp)
  android-log-mcp set-root DIR (--keep-old|--delete-old)
                                               move the data root for every client (android-log-mcp set-root -h)
  android-log-mcp service [status|stop]        the background service (see android-log-mcp service -h)
  android-log-mcp install [--json]             copy this binary to a fixed path for MCP clients to run directly
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
		case "call", "report", "compat", "root", "service", "set-root", "install", "record":
		default:
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp -h", "value", flag.Arg(0), "msg", "unknown_command"))
			os.Exit(2)
		}
	}
	if flag.NArg() > 0 && flag.Arg(0) == "root" {
		os.Exit(runRoot(flag.Args()[1:]))
	}
	if flag.NArg() > 0 && flag.Arg(0) == "install" {
		os.Exit(runInstall(flag.Args()[1:]))
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
	if flag.NArg() > 0 && flag.Arg(0) == "record" {
		if *offline != "" {
			fmt.Fprintln(os.Stderr, "android-log-mcp: --offline cannot be combined with record")
			os.Exit(2)
		}
		code := runRecord(ctx, flag.Args()[1:])
		cancel()
		os.Exit(code)
	}
	if flag.NArg() > 0 && flag.Arg(0) == "report" {
		// through the service, which it starts detached if none runs
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
		raw, ok := callJSON(flag.Args()[1:])
		if !ok {
			os.Exit(2)
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
	cl := service.NewClient(service.ClientOptions{Version: mcpserver.Version, Role: role, Cfg: store.ConfigFromEnv(), RestartPinned: role == "mcp"})
	defer cl.Close()
	if role == "call" {
		raw, ok := callJSON(args[1:])
		if !ok {
			return 2
		}
		h, err := cl.Connect(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("service_unreachable", "android-log-mcp service status", "msg", err.Error()))
			return 1
		}
		if cl.Started() { // the only stderr line of a normal call
			fmt.Fprintln(os.Stderr, mcpserver.KV("service", "state", "started", "pid", h.PID, "version", h.Version))
		}
		if v := os.Getenv("ANDROID_LOG_MCP_CALL_WAIT"); v != "" {
			// deprecated (issue #11): every log tool call waits for its
			// device's catch-up itself
			fmt.Fprintf(os.Stderr, "android-log-mcp: ANDROID_LOG_MCP_CALL_WAIT=%s is deprecated and ignored: calls wait for the backfill themselves (up to 10 s)\n", v)
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

// callJSON is the TOOL [JSON] of `call`: args[0] is the tool, args[1] the
// optional JSON object. A usage or JSON mistake prints one coded line with an
// executable example (exit 2), never a Go error or the usage block.
func callJSON(args []string) (json.RawMessage, bool) {
	const example = `android-log-mcp call logcat_status '{"device":"avd-api29"}'`
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", example, "msg", "missing_tool"))
		return nil, false
	}
	if len(args) == 1 {
		return json.RawMessage("{}"), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args[1]), &obj); err != nil || obj == nil {
		kv := []any{"msg", "not_a_json_object"}
		var se *json.SyntaxError
		if errors.As(err, &se) {
			kv = []any{"col", int(se.Offset), "msg", "syntax"}
		}
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_json", example, kv...))
		return nil, false
	}
	return json.RawMessage(args[1]), true
}
