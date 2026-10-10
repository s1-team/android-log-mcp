package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/report"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseSize accepts 52428800, 50MB, 50M, 50MiB, 512k …
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
sensitive values redacted (on by default). Reads the data directory only;
does not change the device (except --am-start, which launches the app).

flags:
`

func runReport(ctx context.Context, home string, args []string) int {
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
	maxSize := fs.String("max-size", "50MB", "cap on uncompressed log data; the oldest records are dropped first (noted in manifest.json)")
	note := fs.String("note", "", "problem description / repro steps to include (@FILE reads a file)")
	amStart := fs.String("am-start", "", "`PKG/ACTIVITY`: run \"am start -W -n\" and include its output (LAUNCHES THE APP; only when you ask for it)")
	fs.Var(&extra, "redact-extra", "additional redaction regex (RE2; repeatable; group 1 if present, else the whole match)")
	fs.Var(&keepDom, "keep-domain", "`DOMAIN` (and its subdomains) to leave unredacted, e.g. your own API host (repeatable)")
	noRedact := fs.Bool("no-redact", false, "disable redaction (the report may then contain tokens, passwords, personal data)")
	keepPrivate := fs.Bool("keep-private-ip", false, "do not redact private/link-local IP addresses (10/8, 192.168/16, fe80::, …)")
	offline := fs.Bool("no-adb", false, "do not use adb (no live device info, DropBox or --am-start)")
	out := fs.String("o", "", "output zip (default ./android-log-mcp-report-<time>.zip)")
	fs.StringVar(out, "out", "", "same as -o")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "report: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	opt := report.Options{Base: home, Serials: serials, From: *from, To: *to, Package: *pkg, AmStart: *amStart, Out: *out,
		Version: mcpserver.Version, Args: append([]string{"report"}, args...)}
	opt.OwnersDir = layout.DefaultOwnersDir()
	if *from == "" {
		d, err := time.ParseDuration(*since)
		if err != nil {
			if n, err2 := strconv.ParseFloat(strings.TrimSuffix(*since, "d"), 64); err2 == nil && strings.HasSuffix(*since, "d") {
				d = time.Duration(n * 24 * float64(time.Hour))
			} else {
				fmt.Fprintf(os.Stderr, "report: --since %q: expected a duration like 30m, 2h, 1d\n", *since)
				return 2
			}
		}
		opt.Since = d
	}
	var err error
	if opt.MaxBytes, err = parseSize(*maxSize); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 2
	}
	opt.Note = *note
	if strings.HasPrefix(opt.Note, "@") {
		b, err := os.ReadFile(opt.Note[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "report: --note:", err)
			return 2
		}
		opt.Note = string(b)
	}
	opt.Redact.Disabled, opt.Redact.KeepPrivateIP, opt.Redact.Extra = *noRedact, *keepPrivate, extra
	opt.Redact.KeepDomains = keepDom
	opt.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "report: "+f+"\n", a...) }
	// mask --note / --redact-extra / --keep-domain values in the manifest's command line
	opt.Args = maskArgs(opt.Args)
	if !*offline {
		if p, err := device.FindADB(); err == nil {
			adb := &device.ADB{Path: p}
			if devs, err := adb.Output(ctx, 10*time.Second, "devices"); err == nil {
				opt.Online = map[string]bool{}
				for _, d := range device.ParseDevices(devs) {
					if d.State == "device" {
						opt.Online[d.Serial] = true
					}
				}
				opt.Shell = func(ctx context.Context, serial, cmd string) (string, error) {
					return adb.Shell(ctx, serial, 60*time.Second, cmd)
				}
			}
		} else {
			opt.Logf("adb not found (%v): live device info and DropBox are skipped", err)
		}
	}
	if *amStart != "" && opt.Shell == nil {
		fmt.Fprintln(os.Stderr, "report: --am-start needs adb (and not --no-adb)")
		return 2
	}
	if opt.Out == "" {
		opt.Out = "android-log-mcp-report-" + time.Now().Format("20060102-150405") + ".zip"
	}
	man, err := report.Build(ctx, opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	for _, d := range man.Devices {
		fmt.Fprintf(os.Stderr, "report: %s: %d records (%s … %s), %d crashes/ANRs, device info: %s\n", d.Serial, d.Records, d.FirstRecord, d.LastRecord, d.Crashes, d.DeviceInfo)
	}
	if man.Redaction.Enabled {
		fmt.Fprintf(os.Stderr, "report: redacted: %s\n", fmtHits(man.Redaction.Hits))
	}
	for _, w := range man.Warnings {
		fmt.Fprintln(os.Stderr, "report: warning:", w)
	}
	fmt.Fprintln(os.Stderr, "report: review the zip before sharing it: redaction is pattern based")
	fmt.Println(opt.Out)
	return 0
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
