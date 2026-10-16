package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseSize accepts 52428800, 50MB, 50M, 50MiB, 512k …
// defaultMaxSize is the --max-size default (also the value a bad one is
// advised to use).
const defaultMaxSize = "50MB"

func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suf) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suf)), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("--max-size %q: expected a size like 50MB", s)
	}
	return int64(f * float64(mult)), nil
}

const reportUsage = `usage: android-log-mcp report [flags]

Packs recorded logs and diagnostics into a zip for the maintainer, with
sensitive values redacted (on by default). The service process builds
it (it has the data and adb); when none runs, this command starts it
detached. The service starts recording the requested devices and backfills
their log buffers first (up to 10 s; otherwise the manifest says "backfill
incomplete"). Only reads the data; does not change the device (except
--am-start, which launches the app). With --offline DIR it packs that
dataset instead (no adb, no service).

flags:
`

func runReport(ctx context.Context, offlineDir string, args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, reportUsage)
		fs.PrintDefaults()
	}
	var serials, extra, keepDom multiFlag
	fs.Var(&serials, "serial", "device serial to include (repeatable; default: every recorded device)")
	since := fs.String("since", "2h", "window: the last DURATION of device time (ignored with --from)")
	from := fs.String("from", "", `window start, device-local ("2026-10-10 14:00", "14:00", ISO-8601, or "30m" = now minus)`)
	to := fs.String("to", "", "window end (default: up to the newest record)")
	pkg := fs.String("package", "", "keep only this app's lines (and system lines that name it), its crashes, processes and DropBox entries")
	maxSize := fs.String("max-size", defaultMaxSize, "cap on uncompressed log data; the oldest records are dropped first (noted in manifest.json)")
	note := fs.String("note", "", "problem description / repro steps to include (@FILE reads a file)")
	amStart := fs.String("am-start", "", "`PKG/ACTIVITY`: run \"am start -W -n\" and include its output (LAUNCHES THE APP; only when you ask for it)")
	fs.Var(&extra, "redact-extra", "additional redaction regex (RE2; repeatable; group 1 if present, else the whole match)")
	fs.Var(&keepDom, "keep-domain", "`DOMAIN` (and its subdomains) to leave unredacted, e.g. your own API host (repeatable)")
	noRedact := fs.Bool("no-redact", false, "disable redaction (the report may then contain tokens, passwords, personal data)")
	keepPrivate := fs.Bool("keep-private-ip", false, "do not redact private/link-local IP addresses (10/8, 192.168/16, fe80::, …)")
	logsOnly := fs.Bool("logs-only", false, "no recorded device data: only the service log, crash log, service.json, a diagnosis of the service lock and `adb devices -l` (a small zip to attach to an issue; works with --offline DIR when the service is down)")
	offline := fs.Bool("no-adb", false, "do not use adb (no live device info, DropBox or --am-start)")
	out := fs.String("o", "", "output zip (default ./android-log-mcp-report-<time>.zip)")
	fs.StringVar(out, "out", "", "same as -o")
	if code, ok := parseFlags(fs, args, "android-log-mcp report --since 2h"); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp report --since 2h", "value", fs.Arg(0), "msg", "unexpected_argument"))
		return 2
	}
	a := service.ReportArgs{Serials: serials, From: *from, To: *to, Package: *pkg, AmStart: *amStart, Out: *out, NoADB: *offline, LogsOnly: *logsOnly}
	if *from == "" {
		d, err := time.ParseDuration(*since)
		if err != nil {
			if n, err2 := strconv.ParseFloat(strings.TrimSuffix(*since, "d"), 64); err2 == nil && strings.HasSuffix(*since, "d") {
				d = time.Duration(n * 24 * float64(time.Hour))
			} else {
				fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_param", "android-log-mcp report --since 2h", "param", "--since", "value", *since))
				return 2
			}
		}
		a.Since = d
	}
	var err error
	if a.MaxBytes, err = parseSize(*maxSize); err != nil {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_param", "android-log-mcp report --max-size "+defaultMaxSize, "param", "--max-size", "value", *maxSize, "msg", err.Error()))
		return 2
	}
	a.Note = *note
	if strings.HasPrefix(a.Note, "@") {
		b, err := os.ReadFile(a.Note[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_param", "android-log-mcp report", "param", "--note", "value", *note, "msg", err.Error()))
			return 2
		}
		a.Note = string(b)
	}
	a.Redact.Disabled, a.Redact.KeepPrivateIP, a.Redact.Extra = *noRedact, *keepPrivate, extra
	a.Redact.KeepDomains = keepDom
	// mask --note / --redact-extra / --keep-domain values in the manifest's command line
	a.Args = maskArgs(append([]string{"report"}, args...))
	if a.Out == "" {
		a.Out = "android-log-mcp-report-" + time.Now().Format("20060102-150405") + ".zip"
	}
	// the service writes the zip: give it an absolute path
	if a.Out, err = filepath.Abs(a.Out); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 2
	}
	var rep *service.ReportReply
	if offlineDir != "" {
		// --offline DIR report: that dataset, in this process, without adb
		a.NoADB = true
		a.DevicesList = !*offline // only `adb devices -l`, for the bundle
		ctrl := service.DefaultControl()
		if r, _ := service.ConfiguredRoot(ctrl); sameDir(offlineDir, r) {
			a.Control = ctrl // this user's own dataset: say who holds the lock
		}
		if a.AmStart != "" {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", "android-log-mcp report --no-adb", "msg", "am_start_with_offline"))
			return 2
		}
		rep, err = service.BuildReport(ctx, offlineDir, mcpserver.Version, a, nil)
	} else {
		rep, err = service.Report(ctx, service.DefaultControl(), mcpserver.Version, a)
	}
	if rep != nil {
		for _, l := range rep.Log {
			fmt.Fprintln(os.Stderr, mcpserver.KV("report_log", "msg", l))
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("report_failed", "android-log-mcp report --logs-only", "msg", err.Error()))
		return 1
	}
	if rep.PID != os.Getpid() {
		fmt.Fprintln(os.Stderr, mcpserver.KV("report", "built_by", "service", "pid", rep.PID, "version", rep.Version, "root", rep.Root))
	} else {
		fmt.Fprintln(os.Stderr, mcpserver.KV("report", "built_by", "offline", "root", rep.Root))
	}
	man := rep.Manifest
	for _, d := range man.Devices {
		fmt.Fprintln(os.Stderr, mcpserver.KV("report", "device", d.Serial, "records", d.Records, "first", d.FirstRecord, "last", d.LastRecord, "crashes", d.Crashes, "device_info", d.DeviceInfo))
	}
	if man.Redaction.Enabled {
		fmt.Fprintln(os.Stderr, mcpserver.KV("report", "redacted", sumHits(man.Redaction.Hits), "hits", fmtHits(man.Redaction.Hits)))
	}
	for _, w := range man.Warnings {
		fmt.Fprintln(os.Stderr, mcpserver.KV("warning", "msg", w))
	}
	fmt.Fprintln(os.Stderr, "notice: review the zip before sharing it: redaction is pattern based")
	fmt.Println(a.Out)
	return 0
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func fmtHits(h map[string]int) string {
	if len(h) == 0 {
		return "nothing matched"
	}
	var parts []string
	for _, k := range sortedKeys(h) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, h[k]))
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]int) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func maskArgs(a []string) []string {
	out := append([]string(nil), a...)
	for i := 0; i < len(out); i++ {
		name := strings.TrimLeft(out[i], "-")
		key, _, hasVal := strings.Cut(name, "=")
		if key == "note" || key == "redact-extra" || key == "keep-domain" {
			if hasVal {
				out[i] = out[i][:strings.Index(out[i], "=")+1] + "<omitted>"
			} else if i+1 < len(out) {
				out[i+1] = "<omitted>"
				i++
			}
		}
	}
	return out
}

func sumHits(h map[string]int) int {
	n := 0
	for _, v := range h {
		n += v
	}
	return n
}
