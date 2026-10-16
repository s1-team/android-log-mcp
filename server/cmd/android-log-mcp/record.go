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
        independent of tool calls and MCP/CLI processes; the deadline never shortens. Starts the
        service process detached if none runs. Returns without waiting for the backfill.
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
	if code, ok := parseFlags(fs, args, "android-log-mcp record --for 2h"); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp record --for 2h", "value", fs.Arg(0), "msg", "unexpected_argument"))
		return 2
	}
	if *dur != "" && *stop {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp record --stop", "msg", "for_and_stop_exclusive"))
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
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_param", "android-log-mcp record --for 2h", "param", "--for", "value", *dur, "msg", strings.Replace(err.Error(), "record_for", "--for", 1)))
			return 2
		}
		// never touch an unusable record.json
		if _, _, err := service.ReadPins(ctrl, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("pins_unusable", "android-log-mcp service status", "msg", err.Error()))
			return 1
		}
		cl := service.NewClient(service.ClientOptions{Control: ctrl, Version: mcpserver.Version, Role: "record", NoEmbedded: true})
		defer cl.Close()
		var rep service.RecordReply
		if err := cl.Do(ctx, "record", a, &rep); err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("service_error", "android-log-mcp service status", "msg", remoteMsg(err)))
			return 1
		}
		return printRecord(&rep, *asJSON)
	}
	// --stop and the state never start a service
	conn, err := service.Dial(ctx, ctrl, mcpserver.Version, "cli")
	if err == nil {
		defer conn.Close()
		if conn.Hello.Proto < 2 {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("service_too_old", "android-log-mcp service stop", "pid", conn.Hello.PID, "version", conn.Hello.Version))
			return 1
		}
		var rep service.RecordReply
		if err := conn.Do(ctx, "record", a, &rep); err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("service_error", "android-log-mcp service status", "msg", remoteMsg(err)))
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
					return &severalPinnedError{ids: names}
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
			fmt.Fprintln(os.Stderr, mcpserver.CLIErrorAlts("several_devices", severalNext(err), "msg", err.Error()))
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
	rep.Notes = append(rep.Notes, "service not running: nothing is recorded until `android-log-mcp record --for 2h` or the next log tool call")
	return printRecord(rep, asJSON)
}

func printRecord(rep *service.RecordReply, asJSON bool) int {
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	now := time.Now()
	kv := []any{"action", rep.Action}
	if rep.Service != 0 {
		kv = append(kv, "service_pid", rep.Service, "version", rep.Version)
	} else {
		kv = append(kv, "service", "not_running")
	}
	if rep.Deadline != nil {
		kv = append(kv, "pinned_until", rep.Deadline.Format(isoSec), "in_s", int(rep.Deadline.Sub(now).Seconds()))
	}
	word := "ok"
	if len(rep.Devices) == 0 {
		word = "empty"
	}
	fmt.Println(mcpserver.KV(word+" record", kv...))
	pending := ""
	for _, d := range rep.Devices {
		kv := []any{"id", d.ID, "state", d.State, "caught_up", d.CaughtUp}
		if d.Recording && !d.CaughtUp {
			kv = append(kv, "backfill_lines", d.Lines)
			if pending == "" {
				pending = d.ID
			}
		}
		if d.IdleStop != nil {
			kv = append(kv, "idle_stop", d.IdleStop.Format(isoSec))
		}
		if d.Pin != nil {
			kv = append(kv, "pinned_until", d.Pin.Deadline.Format(isoSec), "pin_by", d.Pin.SetBy)
			if d.Pin.Suspended != "" {
				kv = append(kv, "pin_suspended", d.Pin.Suspended)
			}
			if d.Pin.LastError != "" {
				kv = append(kv, "pin_error", d.Pin.LastError)
			}
		} else {
			kv = append(kv, "pinned_until", "-")
		}
		fmt.Println(mcpserver.KV("device", kv...))
	}
	for _, p := range rep.Pins {
		if strings.HasPrefix(p, "pins: ") {
			fmt.Println(mcpserver.KV("note", "code", "pins", "msg", strings.TrimPrefix(p, "pins: ")))
		}
	}
	for _, n := range rep.Notes {
		fmt.Println(mcpserver.KV("note", "msg", n))
	}
	if rep.Action == "pinned" && pending != "" {
		fmt.Println(mcpserver.KV("hint", "code", "backfill_in_progress") + " next: android-log-mcp record")
	}
	return 0
}

const isoSec = "2006-01-02T15:04:05Z07:00"

// severalPinnedError: --stop without --device while several devices are pinned.
type severalPinnedError struct{ ids []string }

func (e *severalPinnedError) Error() string {
	return fmt.Sprintf("%d devices are pinned; pass --device one of: %s", len(e.ids), strings.Join(e.ids, ", "))
}

// severalNext is one standalone command per pinned device (each becomes its
// own `next:` line: they must never run together).
func severalNext(err error) []string {
	var se *severalPinnedError
	if !errors.As(err, &se) {
		return []string{"android-log-mcp record"}
	}
	var cmds []string
	for _, id := range se.ids {
		cmds = append(cmds, "android-log-mcp record --stop --device "+shellQuote(id))
	}
	return cmds
}
