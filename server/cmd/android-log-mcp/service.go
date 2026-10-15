package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

const serviceUsage = `usage:
  android-log-mcp service                run the background service in the foreground
                                         (clients normally start it themselves, detached)
  android-log-mcp service status [--json] [--verbose]
                                         show the running service (default: pid, version, root; --verbose: all)
  android-log-mcp service stop [--force] stop it (clients restart it when they need it);
                                         --force also ends a service that holds the lock but
                                         does not answer (SIGTERM, then SIGKILL; Windows:
                                         TerminateProcess, so unflushed lines may be lost)
`

func runServiceCmd(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return serviceStatus(args[1:])
		case "stop":
			force := false
			for _, a := range args[1:] {
				if a == "--force" || a == "-force" {
					force = true
				} else {
					fmt.Fprintf(os.Stderr, "service stop: unknown argument %q\n", a)
					return 2
				}
			}
			return service.RunStop(service.DefaultControl(), mcpserver.Version, force, os.Stdout, os.Stderr)
		case "-h", "--help", "help":
			fmt.Fprint(os.Stderr, serviceUsage)
			return 0
		}
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, serviceUsage) }
	detached := fs.Bool("detached", false, "started by a client (internal): log only to the service log")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "service: unknown argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	return runService(*detached)
}

// runService runs the service until it is stopped (signal, `service stop`,
// a newer client, or the linger time after the last client).
func runService(detached bool) int {
	ctrl := service.DefaultControl()
	root, err := service.ConfiguredRoot(ctrl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp service:", err)
		return 1
	}
	lf, err := service.OpenLog(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp service: log:", err)
		return 1
	}
	defer lf.Close()
	if detached {
		log.SetOutput(lf)
		if cf, err := service.CrashFile(root); err == nil {
			debug.SetCrashOutput(cf, debug.CrashOptions{})
			cf.Close()
		}
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
	}
	if v, err := time.ParseDuration(os.Getenv("ALM_TEST_EXIT_GRACE")); err == nil && v > 0 {
		service.ExitGrace = v // tests only: exit sooner when unused
	}
	opt := service.Options{Control: ctrl, Version: mcpserver.Version, Root: root, Cfg: store.ConfigFromEnv(), Log: lf}
	if detached {
		opt.OnRootChange = func(root string) { // set-root: the crash log moves along
			if cf, err := service.CrashFile(root); err == nil {
				debug.SetCrashOutput(cf, debug.CrashOptions{})
				cf.Close()
			}
		}
	}
	s, err := service.Start(opt)
	if errors.Is(err, service.ErrRunning) {
		msg := "a service is already running"
		if st := ctrl.ReadState(); st != nil {
			msg += fmt.Sprintf(" (pid %d, version %s)", st.PID, st.Version)
		}
		fmt.Fprintln(os.Stderr, "android-log-mcp service:", msg)
		return 1
	}
	if err != nil {
		log.Printf("service: %v", err)
		fmt.Fprintln(os.Stderr, "android-log-mcp service:", err)
		return 1
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	if detached {
		signal.Ignore(syscall.SIGHUP)
	}
	select {
	case v := <-sig:
		s.Stop("signal " + v.String())
	case <-s.Done():
	}
	<-s.Done()
	return 0
}

func serviceStatus(args []string) int {
	asJSON, verbose := false, false
	for _, a := range args {
		switch a {
		case "--json", "-json":
			asJSON = true
		case "--verbose", "-verbose":
			verbose = true
		default:
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp service status --json", "value", a, "msg", "unknown_argument"))
			return 2
		}
	}
	ctrl := service.DefaultControl()
	c, err := service.Dial(context.Background(), ctrl, mcpserver.Version, "cli")
	if err != nil {
		if d := service.Diagnose(ctrl); d.Held {
			if asJSON {
				b, _ := json.Marshal(struct {
					Running      bool   `json:"running"`
					Unresponsive bool   `json:"unresponsive"`
					PID          int    `json:"pid,omitempty"`
					Detail       string `json:"detail"`
				}{false, true, d.PID, d.Text(ctrl)})
				fmt.Println(string(b))
			} else {
				fmt.Println(mcpserver.KV("error code=unresponsive", "pid", d.PID, "socket", ctrl.Socket(), "msg", d.Text(ctrl)) + " next: android-log-mcp service stop --force")
			}
			return 1
		}
		if asJSON {
			fmt.Println(`{"running":false}`)
		} else {
			fmt.Println(mcpserver.KV("empty service", "running", false, "socket", ctrl.Socket()) + " next: android-log-mcp record --for 2h")
		}
		return 3
	}
	defer c.Close()
	var info service.Info
	if err := c.Do(context.Background(), "status", nil, &info); err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp service status:", err)
		return 1
	}
	if asJSON {
		b, _ := json.MarshalIndent(struct {
			Running bool `json:"running"`
			service.Info
		}{true, info}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(mcpserver.KV("ok service", "running", true, "pid", info.PID, "version", info.Version, "root", info.Root))
	if verbose {
		fmt.Print(info.Text())
		fmt.Printf("socket: %s\n", info.Socket)
	} else {
		fmt.Println(mcpserver.KV("note", "code", "more_fields", "uptime_s", info.UptimeS, "clients", info.Clients) + " next: android-log-mcp service status --verbose")
	}
	return 0
}
