// Package report builds the `android-log-mcp report` diagnostic zip: a
// trimmed, redacted copy of the recorded store (loadable with --home), the
// same logs re-rendered as raw logcat text (replayable through
// device.Pipeline.ReplayText), device info, DropBox entries, optional
// `am start -W` output, the user's note and a manifest.
//
// It never writes to the data directory and never changes the device; the
// only device-visible action is `am start -W`, run only when AmStart is set.
package report

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/redact"
	"github.com/s1crab/android-log-mcp/server/internal/store"
	"github.com/s1crab/android-log-mcp/server/internal/timeparse"
)

// Format identifies the zip layout (bump on incompatible changes).
const Format = "android-log-mcp-report/1"

// DefaultMaxBytes caps the uncompressed log data in a report.
const DefaultMaxBytes = 50 << 20

// DefaultSince is the default time window.
const DefaultSince = 2 * time.Hour

// DropboxTags are collected for every device (crash / native_crash / anr / tombstone).
var DropboxTags = []string{"data_app_crash", "data_app_native_crash", "data_app_anr", "SYSTEM_TOMBSTONE", "system_app_crash", "system_app_native_crash", "system_app_anr"}

// ShellFunc runs a shell command on a device (adb -s serial shell cmd).
type ShellFunc func(ctx context.Context, serial, cmd string) (string, error)

// Options configures Build.
type Options struct {
	Base     string        // data directory (--home)
	Serials  []string      // devices to include (default: every device with data)
	Since    time.Duration // window = device now - Since … now (default 2h); ignored when From is set
	From, To string        // window bounds in timeparse syntax (device-local)
	Package  string        // keep only this app's lines (plus system lines that name it)
	MaxBytes int64         // cap on uncompressed log data (default 50 MiB); oldest records dropped first
	Note     string        // problem description / repro steps
	AmStart  string        // pkg/Activity: run `am start -W -n` and record its output
	Redact   redact.Options
	Out      string // zip path
	Version  string
	Args     []string // command line, for the manifest

	// Shell talks to live devices; nil = offline (no device info, DropBox or am start).
	Shell ShellFunc
	// Online lists serials currently in state "device" (nil with Shell set: assume all).
	Online map[string]bool
	// Now is the host clock (tests); default time.Now.
	Now func() time.Time
	// CatchUp is how long to wait for the recorder to ingest the am start logs (default 8s).
	CatchUp time.Duration
	Logf    func(format string, a ...any)
}

// Manifest is manifest.json.
type Manifest struct {
	Format      string            `json:"format"`
	Tool        string            `json:"tool"`
	Version     string            `json:"version"`
	GeneratedAt string            `json:"generated_at"` // host time, RFC 3339
	Args        []string          `json:"args,omitempty"`
	Params      Params            `json:"params"`
	Devices     []*DeviceManifest `json:"devices"`
	Limits      Limits            `json:"limits"`
	Redaction   Redaction         `json:"redaction"`
	Files       []FileEntry       `json:"files"` // every file except manifest.json itself
	Load        []string          `json:"how_to_load"`
	Warnings    []string          `json:"warnings,omitempty"`
}

// Params echoes the effective parameters (custom regexes are counted, not stored).
type Params struct {
	Serials       []string `json:"serials"`
	Since         string   `json:"since,omitempty"`
	From          string   `json:"from,omitempty"`
	To            string   `json:"to,omitempty"`
	Package       string   `json:"package,omitempty"`
	MaxBytes      int64    `json:"max_bytes"`
	AmStart       string   `json:"am_start,omitempty"`
	Note          bool     `json:"note"`
	Redact        bool     `json:"redact"`
	KeepPrivateIP bool     `json:"keep_private_ip,omitempty"`
	RedactExtra   int      `json:"redact_extra_rules,omitempty"`
	KeepDomains   int      `json:"keep_domains,omitempty"`
	Offline       bool     `json:"offline,omitempty"`
}

// Limits reports the size cap and what it cost.
type Limits struct {
	MaxBytes       int64  `json:"max_bytes"`
	LogBytes       int64  `json:"log_bytes"` // uncompressed store segments + logcat text actually written
	DroppedRecords int    `json:"dropped_records"`
	DroppedBytes   int64  `json:"dropped_bytes"`
	Note           string `json:"note,omitempty"`
}

// Redaction summarizes what the redactor did.
type Redaction struct {
	Enabled     bool           `json:"enabled"`
	Placeholder string         `json:"placeholder"`
	Hits        map[string]int `json:"hits"`          // replacements per rule type
	Unique      map[string]int `json:"unique_values"` // distinct original values per type
	Rules       []string       `json:"rules"`
	Scope       string         `json:"scope"`
}

// DeviceManifest describes one device's part of the report.
type DeviceManifest struct {
	Serial          string            `json:"serial"`
	Dir             string            `json:"dir"`
	Zone            string            `json:"zone"`
	DeviceNow       string            `json:"device_now"`
	WindowFrom      string            `json:"window_from"`
	WindowTo        string            `json:"window_to,omitempty"` // empty = up to the newest record
	Boots           []string          `json:"boots,omitempty"`
	RecordsInWindow int               `json:"records_in_window"` // after the package filter
	Records         int               `json:"records"`           // written
	DroppedRecords  int               `json:"dropped_records,omitempty"`
	FirstRecord     string            `json:"first_record,omitempty"`
	LastRecord      string            `json:"last_record,omitempty"`
	Crashes         int               `json:"crashes"`
	Procs           int               `json:"procs"`
	Dropbox         map[string]int    `json:"dropbox,omitempty"` // entries per tag in the window
	DropboxErrors   map[string]string `json:"dropbox_errors,omitempty"`
	DeviceInfo      string            `json:"device_info"` // live | recorded meta (device offline)
	AmStart         string            `json:"am_start,omitempty"`
}

// FileEntry is one file in the zip.
type FileEntry struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// DeviceInfo is devices/<dir>/device.json.
type DeviceInfo struct {
	Serial       string   `json:"serial"`
	Source       string   `json:"source"` // live | recorded meta (device offline)
	Fingerprint  string   `json:"ro.build.fingerprint,omitempty"`
	Model        string   `json:"ro.product.model,omitempty"`
	Manufacturer string   `json:"ro.product.manufacturer,omitempty"`
	Release      string   `json:"ro.build.version.release,omitempty"`
	SDK          string   `json:"ro.build.version.sdk,omitempty"`
	TZ           string   `json:"persist.sys.timezone,omitempty"`
	TZOffset     string   `json:"tz_offset,omitempty"` // date +%z
	BootID       string   `json:"boot_id,omitempty"`
	DeviceTime   string   `json:"device_time,omitempty"`
	ClockSkewMs  *int64   `json:"device_minus_host_ms,omitempty"`
	Sessions     []string `json:"recorded_boot_ids,omitempty"`
}

type devWork struct {
	serial string
	dir    string // sanitized
	st     *store.Store
	meta   store.Meta
	loc    *time.Location
	zone   string
	now    time.Time
	skew   time.Duration // device clock - host clock (best estimate)
	fromNs int64
	toNs   int64 // 0 = open
	info   *DeviceInfo
	man    *DeviceManifest
	recs   []*model.Record // newest first while collecting, then oldest first
	sizes  []int64
	inWin  int
}

// Build writes the report zip and returns its manifest.
func Build(ctx context.Context, o Options) (*Manifest, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Since <= 0 {
		o.Since = DefaultSince
	}
	if o.CatchUp <= 0 {
		o.CatchUp = 8 * time.Second
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Out == "" {
		o.Out = "android-log-mcp-report-" + o.Now().Format("20060102-150405") + ".zip"
	}
	red, err := redact.New(o.Redact)
	if err != nil {
		return nil, err
	}
	man := &Manifest{Format: Format, Tool: "android-log-mcp", Version: o.Version, GeneratedAt: o.Now().Format(time.RFC3339), Args: o.Args}
	man.Params = Params{From: o.From, To: o.To, Package: o.Package, MaxBytes: o.MaxBytes, AmStart: o.AmStart, Note: o.Note != "",
		Redact: !o.Redact.Disabled, KeepPrivateIP: o.Redact.KeepPrivateIP, RedactExtra: len(o.Redact.Extra), KeepDomains: len(o.Redact.KeepDomains), Offline: o.Shell == nil}
	if o.From == "" {
		man.Params.Since = o.Since.String()
	}
	if o.AmStart != "" && !reComponent.MatchString(o.AmStart) {
		return nil, fmt.Errorf("--am-start %q: expected <package>/<Activity>, e.g. com.foo/.MainActivity", o.AmStart)
	}

	devs, err := selectDevices(o)
	if err != nil {
		return nil, err
	}
	if o.AmStart != "" {
		if o.Shell == nil {
			return nil, errors.New("--am-start needs adb and an online device")
		}
		if len(devs) != 1 {
			return nil, fmt.Errorf("--am-start needs exactly one device; pass --serial (have %d)", len(devs))
		}
	}
	for _, d := range devs {
		man.Params.Serials = append(man.Params.Serials, d.serial)
	}

	zf, err := newZipFile(o.Out)
	if err != nil {
		return nil, err
	}
	defer zf.abort()

	// 1. live data first: device info (also gives the device clock), am start, DropBox
	dbx := map[*devWork]map[string][]dropboxEntry{}
	for _, d := range devs {
		d.info = deviceInfo(ctx, o, d)
		d.man = &DeviceManifest{Serial: d.serial, Dir: d.dir, DeviceInfo: d.info.Source}
		man.Devices = append(man.Devices, d.man)
		if o.AmStart != "" {
			out, err := runAmStart(ctx, o, d)
			if err != nil {
				man.Warnings = append(man.Warnings, err.Error())
			}
			if out != "" {
				d.man.AmStart = "devices/" + d.dir + "/am-start.txt"
				zf.add(d.man.AmStart, []byte(red.Text(out)))
			}
		}
	}
	// 2. windows, then logs (after am start so its lines are included)
	for _, d := range devs {
		if err := d.window(o); err != nil {
			return nil, err
		}
		d.man.Zone, d.man.DeviceNow = d.zone, d.now.Format("2006-01-02 15:04:05 -0700")
		d.man.WindowFrom = time.Unix(0, d.fromNs).In(d.loc).Format("2006-01-02 15:04:05.000 -0700")
		if d.toNs != 0 {
			d.man.WindowTo = time.Unix(0, d.toNs).In(d.loc).Format("2006-01-02 15:04:05.000 -0700")
		}
		if o.Shell != nil && d.online(o) {
			dbx[d] = fetchDropbox(ctx, o, d)
		}
		d.collect(o)
	}
	dropped, droppedBytes := capRecords(devs, o.MaxBytes)

	// 3. write files (redacting oldest → newest so placeholder numbers follow time)
	var logBytes int64
	for _, d := range devs {
		n, err := d.write(zf, red, o, dbx[d])
		if err != nil {
			return nil, err
		}
		logBytes += n
	}
	man.Limits = Limits{MaxBytes: o.MaxBytes, LogBytes: logBytes, DroppedRecords: dropped, DroppedBytes: droppedBytes}
	if dropped > 0 {
		man.Limits.Note = fmt.Sprintf("size cap %d bytes reached: the %d oldest records in the window were left out (see each device's first_record and the \"report size cap\" gap in its meta.json)", o.MaxBytes, dropped)
		man.Warnings = append(man.Warnings, man.Limits.Note)
	}
	if o.Note != "" {
		zf.add("note.txt", []byte(red.Text(strings.TrimRight(o.Note, "\n")+"\n")))
	}
	zf.add("README.txt", []byte(readme))

	man.Redaction = Redaction{Enabled: red.Enabled(), Placeholder: "[REDACTED:<type>#<n>] — the same value maps to the same n within this report; the mapping key is random and discarded, so values cannot be recovered",
		Hits: red.Stats(), Unique: red.Unique(), Rules: redact.Types(),
		Scope: "log messages, crash/ANR/tombstone texts, DropBox bodies, am start output and the note. Not redacted: timestamps, pid/tid/uid, level, tag, package and process names, device properties."}
	if len(o.Redact.Extra) > 0 {
		man.Redaction.Rules = append(man.Redaction.Rules, "custom")
	}
	if !red.Enabled() {
		man.Warnings = append(man.Warnings, "redaction disabled (--no-redact): the report may contain tokens, passwords and personal data")
	}
	man.Load = []string{
		"unzip REPORT.zip -d r && android-log-mcp --home r/store --offline call logcat_crashes '{}'",
		"or point an MCP client at: android-log-mcp --home r/store --offline",
		"raw logcat text for replay tests: devices/<serial>/logcat/<boot>.txt (device.Pipeline.ReplayText), DropBox: devices/<serial>/dropbox/<tag>.txt (Pipeline.ApplyDropbox)",
	}
	man.Files = zf.files
	zf.add("manifest.json", jsonIndent(man))
	if err := zf.close(); err != nil {
		return nil, err
	}
	return man, nil
}

const readme = `android-log-mcp diagnostic report

manifest.json                 parameters, sizes, redaction counts, warnings
note.txt                      the reporter's description (if given)
store/<serial>/               recorded data in the tool's own format; load with
                                android-log-mcp --home <unzipped>/store --offline
devices/<serial>/device.json  build fingerprint, model, SDK, zone offset, boot id
devices/<serial>/logcat/<boot>.txt
                              the same records as raw "logcat -v threadtime -v epoch -v uid
                              -v usec -v printable" text (replayable)
devices/<serial>/dropbox/<tag>.txt
                              DropBox crash / ANR / tombstone entries in the window
devices/<serial>/am-start.txt output of "am start -W" (only with --am-start)

Sensitive values are replaced by [REDACTED:<type>#<n>] unless the report was
made with --no-redact (see manifest.json "redaction"). Redaction is pattern
based: review the files before sharing them publicly.
`

var reComponent = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+/[A-Za-z0-9_.$]+$`)

func selectDevices(o Options) ([]*devWork, error) {
	dirs := store.ListDeviceDirs(o.Base)
	bySerial := map[string]string{}
	var serials []string
	for _, dir := range dirs {
		st, err := store.OpenReadOnly(dir, store.Config{MemRecords: 1})
		if err != nil || st.Serial == "" {
			continue
		}
		bySerial[st.Serial] = dir
		serials = append(serials, st.Serial)
	}
	sort.Strings(serials)
	want := o.Serials
	if len(want) == 0 {
		want = serials
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("no recorded devices under %s (is this the right --home? run android-log-mcp as an MCP server first so it records)", o.Base)
	}
	var out []*devWork
	seen := map[string]bool{}
	for _, s := range want {
		if seen[s] {
			continue
		}
		seen[s] = true
		dir, ok := bySerial[s]
		if !ok {
			return nil, fmt.Errorf("device %q has no recorded logs under %s (have: %s)", s, o.Base, strings.Join(serials, ", "))
		}
		st, err := store.OpenReadOnly(dir, store.Config{MemRecords: 1})
		if err != nil {
			return nil, err
		}
		m := st.Meta()
		loc, zone := device.ResolveZone(m.TZ, m.TZOffset, o.Now())
		if loc == nil {
			loc, zone = time.Local, "host-local (device zone unknown)"
		}
		out = append(out, &devWork{serial: s, dir: store.SanitizeSerial(s), st: st, meta: m, loc: loc, zone: zone})
	}
	return out, nil
}

func (d *devWork) online(o Options) bool {
	if o.Shell == nil {
		return false
	}
	return o.Online == nil || o.Online[d.serial]
}

const infoCmd = "getprop ro.build.fingerprint; getprop ro.product.model; getprop ro.product.manufacturer; getprop ro.build.version.release; getprop ro.build.version.sdk; getprop persist.sys.timezone; date +%z; cat /proc/sys/kernel/random/boot_id; date +%s"

func deviceInfo(ctx context.Context, o Options, d *devWork) *DeviceInfo {
	m := d.meta
	info := &DeviceInfo{Serial: d.serial, Source: "recorded meta (device offline)", Model: m.Model, Release: m.Release, SDK: m.SDK, TZ: m.TZ, TZOffset: m.TZOffset, BootID: m.LastBootID}
	for _, s := range m.Sessions {
		info.Sessions = append(info.Sessions, s.BootID)
	}
	// device clock estimate from the recorder's last measurement
	if m.ClockHostNs != 0 {
		d.skew = time.Duration(m.ClockOffsetNs)
	}
	d.now = o.Now().Add(d.skew)
	if !d.online(o) {
		d.now = d.now.In(d.loc)
		return info
	}
	t0 := o.Now()
	out, err := o.Shell(ctx, d.serial, infoCmd)
	t1 := o.Now()
	if err != nil {
		info.Source = "recorded meta (adb failed: " + firstLine(err.Error()) + ")"
		d.now = d.now.In(d.loc)
		return info
	}
	l := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(l) < 9 {
		l = append(l, "")
	}
	for i := range l {
		l[i] = strings.TrimSpace(l[i])
	}
	info.Source = "live"
	info.Fingerprint, info.Model, info.Manufacturer, info.Release, info.SDK, info.TZ, info.TZOffset, info.BootID = l[0], l[1], l[2], l[3], l[4], l[5], l[6], l[7]
	if loc, zone := device.ResolveZone(info.TZ, info.TZOffset, t1); loc != nil {
		d.loc, d.zone = loc, zone
	}
	if sec, err := strconv.ParseInt(l[8], 10, 64); err == nil {
		mid := t0.Add(t1.Sub(t0) / 2)
		dev := time.Unix(sec, 5e8)
		skew := dev.Sub(mid).Milliseconds()
		info.ClockSkewMs = &skew
		d.skew = dev.Sub(mid)
		d.now = t1.Add(d.skew)
		info.DeviceTime = dev.In(d.loc).Format("2006-01-02 15:04:05 -0700")
	}
	d.now = d.now.In(d.loc)
	return info
}

func runAmStart(ctx context.Context, o Options, d *devWork) (string, error) {
	if !d.online(o) {
		return "", fmt.Errorf("--am-start: %s is not online; skipped", d.serial)
	}
	o.Logf("running am start -W -n %s on %s (this launches the app)", o.AmStart, d.serial)
	start := o.Now()
	out, err := o.Shell(ctx, d.serial, "am start -W -n '"+o.AmStart+"'")
	text := "$ adb -s " + d.serial + " shell am start -W -n " + o.AmStart + "\n# host time " + start.Format(time.RFC3339) + "\n" + out
	if err != nil {
		text += "\n# error: " + err.Error() + "\n"
		return text, fmt.Errorf("--am-start: %v", err)
	}
	// give the recorder (if running) time to store the launch lines: wait
	// until it has a record logged after am start began (START u0 …)
	startDev := start.Add(d.skew).UnixNano()
	deadline := o.Now().Add(o.CatchUp)
	caught := false
	for {
		if st, err := store.OpenReadOnly(d.st.Dir, store.Config{MemRecords: 1}); err == nil && st.Meta().LastTimeNs >= startDev {
			caught = true
			break
		}
		if !o.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return text, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !caught {
		text += "\n# note: no recorder stored any line logged after am start began (waited " + o.CatchUp.String() + "); the launch may be missing from the logs (is android-log-mcp running as an MCP server?)\n"
	}
	return text, nil
}

func (d *devWork) window(o Options) error {
	if o.From != "" {
		t, err := timeparse.Parse("--from", o.From, d.now)
		if err != nil {
			return err
		}
		d.fromNs = t.UnixNano()
	} else {
		d.fromNs = d.now.Add(-o.Since).UnixNano()
	}
	if o.To != "" {
		t, err := timeparse.Parse("--to", o.To, d.now)
		if err != nil {
			return err
		}
		d.toNs = t.UnixNano()
		if d.toNs <= d.fromNs {
			return fmt.Errorf("--to (%s) is not after the start of the window (%s)", t.Format("2006-01-02 15:04:05"), time.Unix(0, d.fromNs).In(d.loc).Format("2006-01-02 15:04:05"))
		}
	}
	return nil
}

func (d *devWork) inWindow(ns int64) bool {
	return ns >= d.fromNs && (d.toNs == 0 || ns < d.toNs)
}

// PackageMatch: the app's own lines (by resolved package/process) or any line naming it.
func PackageMatch(r *model.Record, pkg string) bool {
	if pkg == "" {
		return true
	}
	return r.Package == pkg || r.Process == pkg || strings.HasPrefix(r.Process, pkg+":") || strings.Contains(r.Msg, pkg)
}

func procMatch(proc, pkg string) bool {
	return pkg == "" || proc == pkg || strings.HasPrefix(proc, pkg+":")
}

// collect gathers window records newest first, stopping once MaxBytes is
// exceeded (the remainder is only counted).
func (d *devWork) collect(o Options) {
	// reopen: the recorder may have appended since the device was selected
	if st, err := store.OpenReadOnly(d.st.Dir, store.Config{}); err == nil {
		d.st = st
		d.meta = st.Meta()
	}
	var total int64
	d.st.Scan(0, 0, true, func(r *model.Record) bool {
		if d.toNs != 0 && r.TimeNs >= d.toNs {
			return true
		}
		if r.TimeNs < d.fromNs {
			// logcat is not strictly time ordered across buffers: look a bit further
			return r.TimeNs >= d.fromNs-int64(time.Minute)
		}
		if !PackageMatch(r, o.Package) {
			return true
		}
		d.inWin++
		if total > o.MaxBytes {
			return true
		}
		sz := recordSize(r)
		total += sz
		d.recs = append(d.recs, r)
		d.sizes = append(d.sizes, sz)
		return true
	})
	d.man.RecordsInWindow = d.inWin
}

// capRecords keeps the newest records across all devices within maxBytes.
func capRecords(devs []*devWork, maxBytes int64) (dropped int, droppedBytes int64) {
	type ref struct {
		d *devWork
		i int
	}
	var all []ref
	for _, d := range devs {
		for i := range d.recs {
			all = append(all, ref{d, i})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].d.recs[all[a].i].TimeNs > all[b].d.recs[all[b].i].TimeNs })
	var total int64
	cut := len(all)
	for k, x := range all {
		if total+x.d.sizes[x.i] > maxBytes {
			cut = k
			break
		}
		total += x.d.sizes[x.i]
	}
	kept := map[ref]bool{}
	for _, x := range all[:cut] {
		kept[x] = true
	}
	for _, d := range devs {
		var recs []*model.Record
		for i, r := range d.recs {
			if kept[ref{d, i}] {
				recs = append(recs, r)
			} else {
				droppedBytes += d.sizes[i]
			}
		}
		// oldest first, in store (seq) order
		sort.Slice(recs, func(a, b int) bool { return recs[a].Seq < recs[b].Seq })
		d.man.DroppedRecords = d.inWin - len(recs)
		dropped += d.man.DroppedRecords
		d.recs = recs
		d.sizes = nil
	}
	return dropped, droppedBytes
}

func recordSize(r *model.Record) int64 {
	b, _ := json.Marshal(r)
	return int64(len(b)+1) + int64(len(LogcatText(r)))
}

// LogcatText renders r as `logcat -v threadtime -v epoch -v uid -v usec
// -v printable` lines (one per message line, same header), the format
// logcat.ParseLine / device.Pipeline.ReplayText read.
func LogcatText(r *model.Record) string {
	hdr := fmt.Sprintf("%10d.%06d", r.TimeNs/1e9, r.TimeNs%1e9/1e3)
	if r.UID >= 0 {
		hdr += fmt.Sprintf(" %5d", r.UID)
	}
	lv := r.Level
	if lv == 0 {
		lv = 'I'
	}
	hdr += fmt.Sprintf(" %5d %5d %c %-8s: ", r.PID, r.TID, byte(lv), r.Tag)
	var b strings.Builder
	for _, ln := range strings.Split(r.Msg, "\n") {
		b.WriteString(hdr)
		b.WriteString(strings.TrimRight(ln, "\r"))
		b.WriteByte('\n')
	}
	return b.String()
}

// write emits store/, logcat/, device.json and dropbox/ for one device and
// returns the log bytes written.
func (d *devWork) write(zf *zipFile, red *redact.Redactor, o Options, dbx map[string][]dropboxEntry) (int64, error) {
	pkg := o.Package
	// renumber seqs 1..N; old → new (lower bound) for crash/session references
	oldSeqs := make([]uint64, len(d.recs))
	out := make([]*model.Record, len(d.recs))
	boots := map[string]bool{}
	var bootOrder []string
	for i, r := range d.recs {
		oldSeqs[i] = r.Seq
		c := *r
		c.Seq = uint64(i + 1)
		c.Msg = red.Text(r.Msg)
		out[i] = &c
		if !boots[c.Boot] {
			boots[c.Boot] = true
			bootOrder = append(bootOrder, c.Boot)
		}
	}
	mapSeq := func(old uint64) uint64 {
		if old == 0 {
			return 0
		}
		return uint64(sort.Search(len(oldSeqs), func(i int) bool { return oldSeqs[i] >= old })) + 1
	}
	base := "store/" + d.dir + "/"
	// segment
	var seg strings.Builder
	for _, r := range out {
		b, err := json.Marshal(r)
		if err != nil {
			return 0, err
		}
		seg.Write(b)
		seg.WriteByte('\n')
	}
	logBytes := int64(seg.Len())
	if len(out) > 0 {
		zf.add(base+fmt.Sprintf("seg-%020d.jsonl", 1), []byte(seg.String()))
	}
	// raw logcat text, one file per boot session
	byBoot := map[string]*strings.Builder{}
	lastBuf := map[string]string{}
	for _, r := range out {
		b := byBoot[r.Boot]
		if b == nil {
			b = &strings.Builder{}
			byBoot[r.Boot] = b
		}
		if r.Buffer != "" && r.Buffer != lastBuf[r.Boot] {
			b.WriteString("--------- switch to " + r.Buffer + "\n")
			lastBuf[r.Boot] = r.Buffer
		}
		b.WriteString(LogcatText(r))
	}
	for _, boot := range bootOrder {
		name := boot
		if name == "" {
			name = "unknown"
		}
		logBytes += int64(byBoot[boot].Len())
		zf.add("devices/"+d.dir+"/logcat/"+name+".txt", []byte(byBoot[boot].String()))
	}
	d.man.Records = len(out)
	d.man.Boots = bootOrder
	if len(out) > 0 {
		d.man.FirstRecord = time.Unix(0, out[0].TimeNs).In(d.loc).Format("2006-01-02 15:04:05.000000")
		d.man.LastRecord = time.Unix(0, out[len(out)-1].TimeNs).In(d.loc).Format("2006-01-02 15:04:05.000000")
	}

	// crashes (latest version of each, in the window, for the package)
	var crashLines [][]byte
	cs := device.LoadCrashes(d.st)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].TimeNs < cs[j].TimeNs })
	for _, c := range cs {
		if !d.inWindow(c.TimeNs) || !(pkg == "" || c.Package == pkg || procMatch(c.Process, pkg)) {
			continue
		}
		x := *c
		x.Seq = mapSeq(c.Seq)
		redactCrash(&x, red)
		b, _ := json.Marshal(&x)
		crashLines = append(crashLines, b)
	}
	d.man.Crashes = len(crashLines)
	zf.add(base+"crashes.jsonl", joinLines(crashLines))

	// processes overlapping the window
	var procLines [][]byte
	for _, p := range device.LoadProcs(d.st) {
		if !procMatch(p.Name, pkg) && p.Package != pkg {
			continue
		}
		if !procOverlaps(p, d) {
			continue
		}
		b, _ := json.Marshal(p)
		procLines = append(procLines, b)
	}
	d.man.Procs = len(procLines)
	zf.add(base+"procs.jsonl", joinLines(procLines))

	// uid → packages (only the app's own uid with --package)
	if pk := device.LoadPackages(d.st); pk != nil {
		if pkg != "" {
			f := map[int][]string{}
			for uid, ps := range pk {
				for _, p := range ps {
					if p == pkg {
						f[uid] = ps
					}
				}
			}
			pk = f
		}
		b, _ := json.Marshal(pk)
		zf.add(base+"packages.json", b)
	}

	// meta: sessions remapped, gaps in the window, plus the cap gap
	m := d.meta
	m.Sessions = nil
	for _, s := range d.meta.Sessions {
		if !boots[s.Boot] {
			continue
		}
		s.FirstSeq = mapSeq(s.FirstSeq)
		if s.FirstSeq == 0 {
			s.FirstSeq = 1
		}
		m.Sessions = append(m.Sessions, s)
	}
	m.Gaps = nil
	for _, g := range d.meta.Gaps {
		if (g.ToNs == 0 || g.ToNs >= d.fromNs) && (d.toNs == 0 || g.FromNs < d.toNs) {
			m.Gaps = append(m.Gaps, g)
		}
	}
	if d.man.DroppedRecords > 0 && len(out) > 0 {
		m.Gaps = append([]store.Gap{{FromNs: d.fromNs, ToNs: out[0].TimeNs, Reason: fmt.Sprintf("report size cap: %d oldest records left out", d.man.DroppedRecords)}}, m.Gaps...)
	}
	if len(out) > 0 {
		m.LastTimeNs = out[len(out)-1].TimeNs
	}
	m.State = ""
	zf.add(base+"meta.json", jsonIndent(m))

	// device.json
	zf.add("devices/"+d.dir+"/device.json", jsonIndent(d.info))

	// DropBox
	if dbx != nil {
		for _, tag := range DropboxTags {
			es := dbx[tag]
			if len(es) == 0 {
				continue
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Drop box contents: %d entries (android-log-mcp report: window and package filtered)\nSearching for: %s\n", len(es), tag)
			for _, e := range es {
				b.WriteString("\n========================================\n")
				b.WriteString(e.header + "\n" + red.Text(e.body) + "\n")
			}
			zf.add("devices/"+d.dir+"/dropbox/"+tag+".txt", []byte(b.String()))
		}
	}
	return logBytes, nil
}

func procOverlaps(p procs.Proc, d *devWork) bool {
	start := p.StartNs
	if start == 0 {
		start = p.SeenNs
	}
	if d.toNs != 0 && start >= d.toNs {
		return false
	}
	return p.EndNs == 0 || p.EndNs >= d.fromNs
}

func redactCrash(c *analysis.Crash, red *redact.Redactor) {
	for _, f := range []*string{&c.Headline, &c.Body, &c.Signature, &c.Traces, &c.CPU, &c.Dropbox, &c.Tombstone, &c.Location, &c.Thread} {
		*f = red.Text(*f)
	}
}

func jsonIndent(v any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
	return []byte(b.String())
}

func joinLines(l [][]byte) []byte {
	var out []byte
	for _, b := range l {
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out
}

type dropboxEntry struct {
	header string
	body   string
}

var reDropboxHeader = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) (\S+)`)

const dropboxSep = "\n========================================\n"

// fetchDropbox reads each tag and keeps entries in the window (and naming the package).
func fetchDropbox(ctx context.Context, o Options, d *devWork) map[string][]dropboxEntry {
	res := map[string][]dropboxEntry{}
	for _, tag := range DropboxTags {
		out, err := o.Shell(ctx, d.serial, "dumpsys dropbox --print "+tag)
		if err == nil && strings.Contains(out, "Permission Denial") {
			err = errors.New("permission denied")
		}
		if err != nil {
			if d.man.DropboxErrors == nil {
				d.man.DropboxErrors = map[string]string{}
			}
			d.man.DropboxErrors[tag] = firstLine(err.Error())
			continue
		}
		es := SplitDropbox(out, d.loc, func(ns int64, body string) bool {
			return d.inWindow(ns) && (o.Package == "" || strings.Contains(body, o.Package))
		})
		res[tag] = es
		if len(es) > 0 {
			if d.man.Dropbox == nil {
				d.man.Dropbox = map[string]int{}
			}
			d.man.Dropbox[tag] = len(es)
		}
	}
	return res
}

// SplitDropbox splits `dumpsys dropbox --print` output into raw entries and keeps those accepted by keep.
func SplitDropbox(out string, loc *time.Location, keep func(ns int64, body string) bool) []dropboxEntry {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if strings.HasPrefix(out, dropboxSep[1:]) {
		out = "\n" + out
	}
	parts := strings.Split(out, dropboxSep)
	var res []dropboxEntry
	for _, p := range parts[1:] {
		hdr, body, _ := strings.Cut(p, "\n")
		m := reDropboxHeader.FindStringSubmatch(strings.TrimSpace(hdr))
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05.999", m[1], loc)
		if err != nil {
			continue
		}
		body = strings.TrimRight(body, "\n")
		if len(body) > analysis.MaxDropboxBody {
			body = body[:analysis.MaxDropboxBody] + "\n…[truncated by android-log-mcp report]"
		}
		if keep(t.UnixNano(), body) {
			res = append(res, dropboxEntry{header: strings.TrimSpace(hdr), body: body})
		}
	}
	return res
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ---- zip writer (to a temp file, renamed on success) ----

type zipFile struct {
	path  string
	tmp   *os.File
	zw    *zip.Writer
	files []FileEntry
	err   error
	done  bool
}

func newZipFile(path string) (*zipFile, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".android-log-mcp-report-*.tmp")
	if err != nil {
		return nil, err
	}
	return &zipFile{path: path, tmp: f, zw: zip.NewWriter(f)}, nil
}

func (z *zipFile) add(name string, data []byte) {
	if z.err != nil {
		return
	}
	w, err := z.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		z.err = err
		return
	}
	if _, err := w.Write(data); err != nil {
		z.err = err
		return
	}
	if name != "manifest.json" {
		z.files = append(z.files, FileEntry{Path: name, Bytes: int64(len(data))})
	}
}

func (z *zipFile) close() error {
	if z.err == nil {
		z.err = z.zw.Close()
	}
	if err := z.tmp.Close(); z.err == nil {
		z.err = err
	}
	if z.err == nil {
		z.err = os.Rename(z.tmp.Name(), z.path)
	}
	if z.err != nil {
		os.Remove(z.tmp.Name())
		return z.err
	}
	z.done = true
	return nil
}

func (z *zipFile) abort() {
	if !z.done {
		z.tmp.Close()
		os.Remove(z.tmp.Name())
	}
}

// Extract unpacks a report zip into dir (for loading/replay); it refuses
// paths that would escape dir.
func Extract(zipPath, dir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if rel, err := filepath.Rel(dir, p); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("bad path in zip: %q", f.Name)
		}
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(p)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
