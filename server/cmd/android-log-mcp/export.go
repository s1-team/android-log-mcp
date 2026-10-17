package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/service"
)

const exportUsage = `usage: android-log-mcp export [flags]

Writes the matching lines of a window of the recorded log to a text file
(logcat -v threadtime -v year form) and prints the logcat_export response
(file, bytes, lines, counts; never log lines). The service process writes the
file (it starts detached when none runs). The selection flags are those of
logcat_search. The file is NOT redacted unless --redact is given (report is
the other way round: redacted unless --no-redact).

Without --out the file goes to <root>/export/ like the logcat_export tool
(kept 7 days, newest 20, 256 MiB). With --out FILE it is written there:
an absolute path, in an existing directory, never inside the data root's or
control directory's own entries (versions, run, tmp, bin, log, export). An
existing file needs --force. With --offline DIR --out is required (the
dataset is never written to).

flags:
`

func runExport(ctx context.Context, offlineDir string, tools *mcpserver.Tools, args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, exportUsage)
		fs.PrintDefaults()
	}
	const example = "android-log-mcp export --min-level W --out /tmp/log.txt"
	var tags, extra, keepDom multiFlag
	device := fs.String("device", "", "device serial or id (default: the only one)")
	filt := fs.String("filter", "", "filter expression, as in logcat_search")
	pkg := fs.String("package", "", "keep only this app's lines")
	fs.Var(&tags, "tag", "keep only lines with this tag (repeatable)")
	minLevel := fs.String("min-level", "", "V, D, I, W, E or F")
	from := fs.String("from", "", "window start, device time")
	to := fs.String("to", "", "window end, device time")
	after := fs.String("after", "", "cursor: only lines newer than this (continues a truncated export)")
	before := fs.String("before", "", "cursor: only lines older than this")
	maxMB := fs.Int("max-mb", 0, "size cap in MiB (default 64, max 512)")
	out := fs.String("out", "", "output file, an absolute path (default: <root>/export/)")
	force := fs.Bool("force", false, "overwrite an existing --out file")
	redactOn := fs.Bool("redact", false, "mask tokens, e-mail and IP addresses etc. in the message text (pattern based, not complete)")
	fs.Var(&extra, "redact-extra", "additional redaction regex (needs --redact; RE2; repeatable)")
	fs.Var(&keepDom, "keep-domain", "`DOMAIN` to leave unredacted (needs --redact; repeatable)")
	keepPrivate := fs.Bool("keep-private-ip", false, "do not redact private IP addresses (needs --redact)")
	if code, ok := parseFlags(fs, args, example); !ok {
		return code
	}
	bad := func(param, msg string) int {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("bad_param", example, "param", param, "msg", msg))
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, mcpserver.CLIError("usage", example, "value", fs.Arg(0), "msg", "unexpected_argument"))
		return 2
	}
	if *out != "" && !filepath.IsAbs(*out) {
		return bad("--out", "must be an absolute path")
	}
	if *force && *out == "" {
		return bad("--force", "only with --out")
	}
	if offlineDir != "" && *out == "" {
		return bad("--out", "required with --offline: a dataset is never written to")
	}
	if !*redactOn && (len(extra) > 0 || len(keepDom) > 0 || *keepPrivate) {
		return bad("--redact", "--redact-extra, --keep-domain and --keep-private-ip need --redact")
	}
	m := map[string]any{}
	set := func(k string, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set("device", *device)
	set("filter", *filt)
	set("package", *pkg)
	set("min_level", *minLevel)
	set("from", *from)
	set("to", *to)
	set("after", *after)
	set("before", *before)
	if len(tags) > 0 {
		m["tags"] = []string(tags)
	}
	if *maxMB != 0 {
		m["max_mb"] = *maxMB
	}
	if *redactOn {
		m["redact"] = true
	}
	if len(extra) > 0 {
		m["redact_extra"] = []string(extra)
	}
	raw, _ := json.Marshal(m)

	var text string
	var isErr bool
	if offlineDir != "" {
		if r, err := layout.ResolvePath(offlineDir); err == nil {
			if o, err := layout.ResolvePath(*out); err == nil && (o == r || layout.Within(o, r)) {
				return bad("--out", "inside the --offline dataset: choose another directory")
			}
		}
		text, isErr = tools.ExportToFile(ctx, raw, mcpserver.ExportTarget{Out: *out, Force: *force, KeepPrivateIP: *keepPrivate, KeepDomains: keepDom})
	} else {
		a := service.ExportArgs{Args: raw, Out: *out, Force: *force, KeepPrivateIP: *keepPrivate, KeepDomains: keepDom}
		rep, err := service.Export(ctx, service.DefaultControl(), mcpserver.Version, a)
		if err != nil {
			fmt.Fprintln(os.Stderr, mcpserver.CLIError("service_unreachable", "android-log-mcp service status", "msg", err.Error()))
			return 1
		}
		text, isErr = rep.Text, rep.IsError
	}
	fmt.Println(text)
	if isErr {
		return 1
	}
	return 0
}
