package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

const recordUsage = `usage:
  android-log-mcp record [--device X]... --for DURATION [--json]
        keep recording the device(s) until now + DURATION (e.g. 30m, 2h, 1h30m; max 24h),
        independent of tool calls and clients; the deadline never shortens. Starts the
        background service detached if none runs. Returns without waiting for the backfill.
  android-log-mcp record --stop [--device X]... [--json]
        stop recording the device(s) now and remove their pins
  android-log-mcp record [--json]
        show what is recorded: per device recording/idle, backfill progress, pin deadline
        (and who set it), idle deadline
Without --device: the single online device (--for) or the single recorded device (--stop).
`

func runRecord(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, recordUsage) }
	var devs multiFlag
	fs.Var(&devs, "device", "device ID or adb serial (repeatable)")
	dur := fs.String("for", "", "pin duration, e.g. 30m, 2h, 1h30m (max 24h)")
	stop := fs.Bool("stop", false, "stop recording now and remove the pin")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "record: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *dur != "" && *stop {
		fmt.Fprintln(os.Stderr, "record: --for and --stop cannot be combined")
		return 2
	}
	ctrl := service.DefaultControl()
	a := service.RecordArgs{Devices: devs, For: *dur, Stop: *stop, By: "android-log-mcp record (pid " + fmt.Sprint(os.Getpid()) + ")"}
	if *dur != "" {
		d, err := mcpserver.ParsePin(*dur)
		if err == nil && d == 0 {
			err = errors.New("--for 0: use --stop to stop recording")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "record:", strings.Replace(err.Error(), "record_for", "--for", 1))
			return 2
		}
		// never touch an unusable record.json
		if _, _, err := service.ReadPins(ctrl, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "record:", err)
			return 1
		}
		cl := service.NewClient(service.ClientOptions{Control: ctrl, Version: mcpserver.Version, Role: "record", NoEmbedded: true})
		defer cl.Close()
		var rep service.RecordReply
		if err := cl.Do(ctx, "record", a, &rep); err != nil {
			fmt.Fprintln(os.Stderr, "record:", remoteMsg(err))
			return 1
		}
		return printRecord(&rep, *asJSON)
	}
	// --stop and the state never start a service
	conn, err := service.Dial(ctx, ctrl, mcpserver.Version, "cli")
	if err == nil {
		defer conn.Close()
		if conn.Hello.Proto < 2 {
			fmt.Fprintf(os.Stderr, "record: the running service (pid %d, version %s) predates recording pins; stop it with `android-log-mcp service stop`\n", conn.Hello.PID, conn.Hello.Version)
			return 1
		}
		var rep service.RecordReply
		if err := conn.Do(ctx, "record", a, &rep); err != nil {
			fmt.Fprintln(os.Stderr, "record:", remoteMsg(err))
			return 1
		}
		return printRecord(&rep, *asJSON)
	}
	return recordOffline(ctrl, a, *asJSON)
}

func remoteMsg(err error) string {
	var re *ipc.RemoteError
	if errors.As(err, &re) {
		return re.Msg
	}
	return err.Error()
}

// recordOffline handles --stop and the state with no service running:
// record.json only (nothing is recorded without a service).
func recordOffline(ctrl service.Control, a service.RecordArgs, asJSON bool) int {
	now := time.Now()
	rep := &service.RecordReply{Action: "state"}
	if a.Stop {
		rep.Action = "stopped"
		_, err := service.UpdatePins(ctrl, now, func(pf *service.PinFile) error {
			names := a.Devices
			if len(names) == 0 {
				for id := range pf.Pins {
					names = append(names, id)
				}
				if len(names) > 1 {
					sort.Strings(names)
					return fmt.Errorf("%d devices are pinned; pass --device one of: %s", len(names), strings.Join(names, ", "))
				}
			}
			for _, n := range names {
				if _, ok := pf.Pins[n]; ok {
					delete(pf.Pins, n)
					rep.Notes = append(rep.Notes, n+": pin removed")
				} else {
					rep.Notes = append(rep.Notes, n+": not pinned")
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "record:", err)
			return 1
		}
	}
	pf, warn, err := service.ReadPins(ctrl, now)
	rep.Notes = append(rep.Notes, warn...)
	if err != nil {
		rep.Notes = append(rep.Notes, err.Error())
	} else {
		ids := make([]string, 0, len(pf.Pins))
		for id := range pf.Pins {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rep.Devices = append(rep.Devices, service.RecordDevice{ID: id, State: "idle", Pin: pf.Pins[id]})
		}
	}
	rep.Notes = append(rep.Notes, "service: not running (nothing is recorded; `record --for` or the next log tool call starts it)")
	return printRecord(rep, asJSON)
}

func printRecord(rep *service.RecordReply, asJSON bool) int {
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	now := time.Now()
	if rep.Service != 0 {
		fmt.Printf("service: pid %d, version %s\n", rep.Service, rep.Version)
	}
	if rep.Action == "pinned" && rep.Deadline != nil {
		fmt.Printf("pinned until %s (in %s)\n", rep.Deadline.Local().Format("2006-01-02 15:04:05"), rep.Deadline.Sub(now).Round(time.Second))
	}
	if len(rep.Devices) == 0 {
		fmt.Println("recording: idle (starts on the first log tool call)")
	}
	pending := false
	for _, d := range rep.Devices {
		line := d.ID + ": " + d.State
		if d.Detail != "" {
			line += " (" + d.Detail + ")"
		}
		if d.Recording && !d.CaughtUp {
			pending = true
		}
		fmt.Println(line)
		if d.Pin != nil {
			fmt.Println("  " + d.Pin.Describe(now))
		}
		if d.IdleStop != nil {
			fmt.Printf("  idle stop at %s unless called again or pinned\n", d.IdleStop.Local().Format("2006-01-02 15:04:05"))
		}
	}
	for _, p := range rep.Pins {
		if strings.HasPrefix(p, "pins: ") {
			fmt.Println(p)
		}
	}
	for _, n := range rep.Notes {
		fmt.Println("note: " + n)
	}
	if rep.Action == "pinned" && pending {
		fmt.Println("backfill in progress; check with: android-log-mcp record")
	}
	return 0
}
