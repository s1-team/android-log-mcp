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
  android-log-mcp service status [--json]
                                         show the running service (pid, version, protocol, root, clients)
  android-log-mcp service stop           stop it (clients restart it when they need it)
`

func runServiceCmd(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return serviceStatus(args[1:])
		case "stop":
			return serviceStop()
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
	asJSON := len(args) > 0 && (args[0] == "--json" || args[0] == "-json")
	ctrl := service.DefaultControl()
	c, err := service.Dial(context.Background(), ctrl, mcpserver.Version, "cli")
	if err != nil {
		if asJSON {
			fmt.Println(`{"running":false}`)
		} else {
			fmt.Printf("service: not running (socket %s)\n", ctrl.Socket())
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
	fmt.Print(info.Text())
	fmt.Printf("socket: %s\n", info.Socket)
	return 0
}

func serviceStop() int {
	ctrl := service.DefaultControl()
	c, err := service.Dial(context.Background(), ctrl, mcpserver.Version, "cli")
	if err != nil {
		fmt.Println("service: not running")
		return 0
	}
	pid := c.Hello.PID
	err = c.Do(context.Background(), "stop", nil, nil)
	c.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "android-log-mcp service stop:", err)
		return 1
	}
	if !service.WaitStopped(ctrl, 20*time.Second) {
		fmt.Fprintf(os.Stderr, "android-log-mcp service stop: pid %d is still running after 20 s\n", pid)
		return 1
	}
	fmt.Printf("service: stopped (pid %d)\n", pid)
	return 0
}
