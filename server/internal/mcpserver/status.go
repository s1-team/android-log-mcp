package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func deviceDesc(d *device.Device) string {
	m := d.Store.Meta()
	st := d.Status().State
	if st == "" {
		st = "not connected"
	}
	model := m.Model
	if model == "" {
		model = "?"
	}
	return fmt.Sprintf("%s (%s, %s)", d.Serial, strings.ReplaceAll(model, " ", "_"), st)
}

// resolveDevice picks the device for a query.
func (t *Tools) resolveDevice(serial string) (*device.Device, error) {
	t.M.WaitReady(3 * time.Second)
	devs := t.M.Devices()
	live := t.M.LiveStates()
	if serial != "" {
		if d := t.M.Get(serial); d != nil {
			return d, nil
		}
		if st, ok := live[serial]; ok {
			return nil, stateError(serial, st)
		}
		if len(devs) == 0 {
			return nil, errf("device %q has no recorded logs and is not connected. %s", serial, t.noDeviceHint())
		}
		var names []string
		for _, d := range devs {
			names = append(names, deviceDesc(d))
		}
		return nil, errf("device %q has no recorded logs. Devices with logs: %s", serial, strings.Join(names, ", "))
	}
	var online, withData []*device.Device
	for _, d := range devs {
		if d.Status().State == "device" {
			online = append(online, d)
		}
		if d.Store.LastSeq() > 0 {
			withData = append(withData, d)
		}
	}
	switch {
	case len(online) == 1:
		return online[0], nil
	case len(online) == 0 && len(withData) == 1:
		return withData[0], nil
	case len(online) > 1 || len(withData) > 1:
		cand := online
		if len(cand) == 0 {
			cand = withData
		}
		var names []string
		for _, d := range cand {
			names = append(names, deviceDesc(d))
		}
		return nil, errf("%d devices have logs; pass device=one of: %s.", len(cand), strings.Join(names, ", "))
	}
	for s, st := range live {
		if st != "device" {
			return nil, stateError(s, st)
		}
	}
	return nil, errf("%s", t.noDeviceHint())
}

func stateError(serial, st string) error {
	switch st {
	case "unauthorized":
		return errf("Device %s is unauthorized: accept the \"Allow USB debugging\" prompt on the phone.", serial)
	case "offline":
		return errf("Device %s is offline (adb state). Reconnect the cable or restart adb (`adb kill-server`).", serial)
	}
	return errf("Device %s is in adb state %q and cannot be recorded yet.", serial, st)
}

func (t *Tools) noDeviceHint() string {
	if err := t.M.ADBError(); err != nil {
		return err.Error()
	}
	if err := t.M.WatchError(); err != nil {
		return "adb server problem: " + err.Error()
	}
	msg := "No device connected. Connect a device with USB debugging enabled or start an emulator; logcat_status shows the state."
	var prev []string
	for _, d := range t.M.Devices() {
		m := d.Store.Meta()
		prev = append(prev, fmt.Sprintf("%s (last seen %s)", d.Serial, time.Unix(0, m.LastSeenHostNs).Format("2006-01-02 15:04:05")))
	}
	if len(prev) > 0 {
		msg += " Previously recorded devices: " + strings.Join(prev, ", ") + " — pass device=\"<serial>\" to query stored logs."
	}
	return msg
}

func (t *Tools) status(ctx context.Context, a statusArgs) (string, error) {
	t.M.WaitReady(3 * time.Second)
	var b strings.Builder
	if err := t.M.ADBError(); err != nil {
		b.WriteString("adb: NOT FOUND — " + err.Error() + "\n")
	} else if adb := t.M.ADB(); adb != nil {
		v, err := adb.Version(ctx)
		if err != nil {
			fmt.Fprintf(&b, "adb: error — %v\n", err)
		} else {
			fmt.Fprintf(&b, "adb: ok (%s, version %s)\n", adb.Path, v)
		}
		if werr := t.M.WatchError(); werr != nil {
			fmt.Fprintf(&b, "adb server: %v\n", werr)
		}
	}
	devs := t.M.Devices()
	live := t.M.LiveStates()
	if a.Device != "" {
		d := t.M.Get(a.Device)
		if d == nil {
			_, err := t.resolveDevice(a.Device)
			return "", err
		}
		devs = []*device.Device{d}
	}
	fmt.Fprintf(&b, "store: %s (data in %s/<device-id>)\n", t.M.Base, filepath.Base(store.DataDir(t.M.Base)))
	if t.M.HomeReadOnly() {
		fmt.Fprintf(&b, "mode: %s\n", t.M.ReadOnlyReason())
	}
	fmt.Fprintf(&b, "devices (%d):\n", len(devs))
	known := map[string]bool{}
	for _, d := range devs {
		known[d.Serial] = true
		if st := d.Status(); st.State != "" {
			known[d.Addr()] = true
		}
		d.Store.Refresh()
		m := d.Store.Meta()
		st := d.Status()
		loc := d.Loc()
		state := st.State
		if state == "" {
			state = "not connected"
		}
		rel := ""
		if m.Release != "" {
			rel = fmt.Sprintf("  android %s (sdk %s)", m.Release, m.SDK)
		}
		via := ""
		if st.State != "" && d.Addr() != d.Serial {
			via = " (adb " + d.Addr() + ")"
		}
		fmt.Fprintf(&b, "  %s%s  %s  %s%s  tz %s\n", d.Serial, via, state, orQ(m.Model), rel, tzLabel(loc, d.TZName(), time.Now()))
		stats := d.Store.Stats()
		var rec string
		switch {
		case st.Owner != nil && st.Owner.Legacy:
			rec = fmt.Sprintf("NOT recorded by this process: an older android-log-mcp (pid %d, pre-0.4 flat layout) records it into %s; this process only reads that data. Quit/upgrade the old one and this process takes over (data is migrated).", st.Owner.PID, st.Owner.DataDir)
		case st.Owner != nil:
			rec = "NOT recorded by this process: owned by another android-log-mcp process (" + st.Owner.String() + "); this process only reads that data"
		case t.M.HomeReadOnly():
			rec = "NOT recorded: this process is read-only (ANDROID_LOG_MCP_HOME/--home is set) and never records; showing data from " + d.Store.Dir
		case st.Paused != "":
			rec = st.Paused
		case st.Recording:
			rec = "recording (this process)"
		case st.State == "device" && !st.Owned && !t.M.NoADB:
			rec = fmt.Sprintf("NOT recorded: recording limit of %d devices reached (ANDROID_LOG_MCP_MAX_DEVICES)", t.M.MaxDevices)
		case st.State == "device" && !st.Writer:
			rec = "recorded by another android-log-mcp process"
			if st.WriterPID != "" {
				rec += " (pid " + st.WriterPID + ")"
			}
		case st.State == "device":
			rec = "starting recorder"
		case !t.M.NoADB && filepath.Dir(d.Store.Dir) != filepath.Clean(store.DataDir(t.M.Base)):
			rec = "not recording: data from an older version's layout (read-only; deleted once older than the retention window)"
		default:
			rec = "not recording (device not connected)"
		}
		if st.LastErr != "" {
			rec += "; last error: " + st.LastErr
		}
		fmt.Fprintf(&b, "    %s\n", rec)
		if st.IDNote != "" {
			fmt.Fprintf(&b, "    note: %s\n", st.IDNote)
		}
		line := ""
		if st.State == "device" {
			line = "device time now: " + d.Now().Format("2006-01-02 15:04:05")
			if off := time.Duration(m.ClockOffsetNs); off > 5*time.Second || off < -5*time.Second {
				dir := "ahead of"
				if off < 0 {
					dir, off = "behind", -off
				}
				line += fmt.Sprintf(" (device clock is %s %s this host; re-measured every 30 s)", off.Round(time.Second), dir)
			}
			line += "   "
		}
		if m.RecordingSince != 0 { // host time → device clock
			line += "recording since " + time.Unix(0, m.RecordingSince+m.ClockOffsetNs).In(loc).Format("2006-01-02 15:04:05") + "   "
		}
		if stats.OldestNs != 0 {
			line += "history from " + dateTime(stats.OldestNs, loc) + " to " + dateTime(stats.NewestNs, loc)
		}
		if line != "" {
			fmt.Fprintf(&b, "    %s\n", strings.TrimSpace(line))
		}
		cfg := d.Store.Config()
		fmt.Fprintf(&b, "    stored %s lines (%.1f MB; retention %s, max %d MiB per device)", commas(int(stats.Records)), float64(stats.Bytes)/1e6, store.FormatRetention(cfg.MaxAge), cfg.MaxBytes>>20)
		if len(m.Gaps) > 0 {
			fmt.Fprintf(&b, "; gaps: %d (latest: %s)", len(m.Gaps), gapText(m.Gaps[len(m.Gaps)-1], loc))
		}
		b.WriteString("\n")
		win := fmt.Sprintf("%s lines (~%.1f MB)", commas(stats.MemRecords), float64(stats.MemBytes)/1e6)
		if !stats.MemLoaded {
			win = "not loaded (loads on the next query)"
		}
		fmt.Fprintf(&b, "    memory: recent window %s; history cache ~%.1f MB\n", win, float64(stats.CacheBytes)/1e6)
		if note := lagNote(d, st, m, stats); note != "" {
			fmt.Fprintf(&b, "    note: %s\n", note)
		}
		if hostLoc := time.Local; st.State == "device" {
			_, ho := time.Now().In(hostLoc).Zone()
			_, do := time.Now().In(loc).Zone()
			if ho != do {
				fmt.Fprintf(&b, "    note: host timezone differs (%s); all times here are device-local\n", tzLabel(hostLoc, "host", time.Now()))
			}
		}
	}
	for s, stt := range live {
		if !known[s] && (a.Device == "" || a.Device == s) {
			fmt.Fprintf(&b, "  %s  %s  (not recorded: ", s, stt)
			if stt == "unauthorized" {
				b.WriteString("accept the \"Allow USB debugging\" prompt on the phone)\n")
			} else if stt == "device" && t.M.HomeReadOnly() {
				fmt.Fprintf(&b, "this process is read-only because ANDROID_LOG_MCP_HOME/--home is set; nobody records it — run android-log-mcp without it)\n")
			} else if stt == "device" && t.M.AddrOf(s) == "" {
				b.WriteString("identifying the device…)\n")
			} else if stt == "device" {
				fmt.Fprintf(&b, "recording limit of %d devices reached; set ANDROID_LOG_MCP_MAX_DEVICES)\n", t.M.MaxDevices)
			} else {
				b.WriteString("adb state not ready)\n")
			}
		}
	}
	if len(devs) == 0 && len(live) == 0 {
		b.WriteString("  (none) — " + t.noDeviceHint() + "\n")
	}
	// cursor + package for the resolved device
	d, err := t.resolveDevice(a.Device)
	if err != nil {
		if a.Package != "" {
			return "", err
		}
		return b.String(), nil
	}
	m := d.Store.Meta()
	boot := "0"
	if len(m.Sessions) > 0 {
		boot = m.Sessions[len(m.Sessions)-1].Boot
	}
	cur := store.Cursor{Serial: d.Serial, Boot: boot, Seq: d.Store.LastSeq()}
	fmt.Fprintf(&b, "cursor (now, %s): %s\n", d.Serial, cur)
	if a.Package != "" {
		b.WriteString(t.packageStatus(d, a.Package))
	}
	return b.String(), nil
}

// lagNote explains a stale newest line: a hung writer, logcat still
// delivering a backlog (device under load), or simply a quiet device.
func lagNote(d *device.Device, st device.Status, m store.Meta, stats store.Stats) string {
	if st.State != "device" || stats.NewestNs == 0 {
		return ""
	}
	behind := time.Duration(d.Now().UnixNano() - stats.NewestNs)
	if behind < 15*time.Second {
		return ""
	}
	if !st.Writer && m.LastSeenHostNs != 0 {
		if age := time.Since(time.Unix(0, m.LastSeenHostNs)); age > 75*time.Second { // heartbeat every 30 s
			return fmt.Sprintf("the recording process (pid %s) has not written anything for %s (stuck or exiting?); newest line is %s old", orQ(st.WriterPID), age.Round(time.Second), behind.Round(time.Second))
		}
	}
	if m.IngestHostNs != 0 && time.Since(time.Unix(0, m.IngestHostNs)) < 5*time.Second {
		return fmt.Sprintf("newest stored line is %s behind the device clock although lines are still arriving: logcat is delivering a backlog (device under load); it will catch up", behind.Round(time.Second))
	}
	return fmt.Sprintf("newest stored line is %s old (device quiet, or logcat delivery stalled)", behind.Round(time.Second))
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return strings.ReplaceAll(s, " ", "_")
}

func gapText(g store.Gap, loc *time.Location) string {
	to := "ongoing"
	if g.ToNs > 0 {
		to = dateTime(g.ToNs, loc)
	}
	from := "?"
	if g.FromNs > 0 {
		from = dateTime(g.FromNs, loc)
	}
	return fmt.Sprintf("%s–%s, %s", from, to, g.Reason)
}

func (t *Tools) packageStatus(d *device.Device, pkg string) string {
	var b strings.Builder
	loc := d.Loc()
	pk := d.Packages()
	installed, uid := "unknown (package list not loaded yet)", -1
	if len(pk) > 0 {
		installed = "not installed"
		for u, l := range pk {
			for _, p := range l {
				if p == pkg {
					installed, uid = "installed", u
				}
			}
		}
	}
	if uid >= 0 {
		fmt.Fprintf(&b, "package %s: %s (uid %d)\n", pkg, installed, uid)
	} else {
		fmt.Fprintf(&b, "package %s: %s\n", pkg, installed)
		if installed == "not installed" {
			if s := similar(pkg, allPkgs(pk)); len(s) > 0 {
				fmt.Fprintf(&b, "  did you mean: %s\n", strings.Join(s, ", "))
			}
		}
	}
	m := d.Store.Meta()
	boot := ""
	if len(m.Sessions) > 0 {
		boot = m.Sessions[len(m.Sessions)-1].Boot
	}
	connected := d.Status().State == "device"
	var alive, dead, prior []procs.Proc
	for _, p := range d.Procs() {
		// app processes only: helpers running with the app's uid
		// (crash_dump64, sh …) have no package and are not listed
		if !(p.Package == pkg || p.Name == pkg || strings.HasPrefix(p.Name, pkg+":")) {
			continue
		}
		switch {
		case p.Boot != boot:
			prior = append(prior, p)
		case p.Alive():
			alive = append(alive, p)
		default:
			dead = append(dead, p)
		}
	}
	startOf := func(p procs.Proc) int64 {
		if p.StartNs != 0 {
			return p.StartNs
		}
		return p.SeenNs
	}
	sort.Slice(alive, func(i, j int) bool { return startOf(alive[i]) > startOf(alive[j]) })
	sort.Slice(dead, func(i, j int) bool { return dead[i].EndNs > dead[j].EndNs })
	sort.Slice(prior, func(i, j int) bool { return startOf(prior[i]) > startOf(prior[j]) })
	crashes := d.Crashes()
	crashOf := func(p procs.Proc) string {
		for _, c := range crashes {
			if c.PID == p.PID && c.Boot == p.Boot && c.TimeNs >= startOf(p)-5e9 && (p.EndNs <= 0 || c.TimeNs <= p.EndNs+5e9) {
				if c.Kind == "anr" {
					return "; ANR: see logcat_crashes id=" + c.ID
				}
				return "; crashed: see logcat_crashes id=" + c.ID
			}
		}
		return ""
	}
	life := func(p procs.Proc) string {
		start := "before recording"
		if p.StartNs > 0 {
			start = clock(p.StartNs, loc)[:8]
		} else if p.SeenNs > 0 {
			start = "≤" + clock(p.SeenNs, loc)[:8]
		}
		end := "?"
		if p.EndNs > 0 {
			end = clock(p.EndNs, loc)[:8]
		}
		return fmt.Sprintf("pid %d %s (%s–%s, %s%s)", p.PID, p.Name, start, end, orStr(p.EndReason, "ended"), crashOf(p))
	}
	switch {
	case len(alive) > 0:
		var parts []string
		for _, p := range alive {
			since := "before recording"
			if p.StartNs > 0 {
				since = dateTime(p.StartNs, loc)
			} else if p.SeenNs > 0 {
				since = "at least " + dateTime(p.SeenNs, loc) + " (seen in ps)"
			}
			parts = append(parts, fmt.Sprintf("pid %d %s (since %s)", p.PID, p.Name, since))
		}
		label := "running"
		if !connected {
			label = "running when the device was last seen"
		}
		fmt.Fprintf(&b, "  %s: %s\n", label, strings.Join(parts, ", "))
	default:
		msg := fmt.Sprintf("  %s has no running process", pkg)
		if len(dead) > 0 {
			msg += fmt.Sprintf(" (last pid %d ended %s)", dead[0].PID, dateTime(dead[0].EndNs, loc))
		}
		b.WriteString(msg + ".\n")
	}
	if len(dead) > 0 {
		var parts []string
		for i, p := range dead {
			if i == 6 {
				parts = append(parts, fmt.Sprintf("… %d more", len(dead)-6))
				break
			}
			parts = append(parts, life(p))
		}
		fmt.Fprintf(&b, "  ended this boot: %s\n", strings.Join(parts, "; "))
	}
	if len(prior) > 0 {
		var parts []string
		for i, p := range prior {
			if i == 6 {
				parts = append(parts, fmt.Sprintf("… %d more", len(prior)-6))
				break
			}
			if p.Alive() { // never closed (older store): the reboot ended it
				p.EndReason = "device rebooted"
			}
			parts = append(parts, life(p)+" [boot "+p.Boot+"]")
		}
		fmt.Fprintf(&b, "  before the last reboot: %s\n", strings.Join(parts, "; "))
	}
	return b.String()
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func allPkgs(m map[int][]string) []string {
	var out []string
	for _, l := range m {
		out = append(out, l...)
	}
	return out
}

// similar returns up to 3 candidates close to s.
func similar(s string, cands []string) []string {
	type sc struct {
		c string
		d int
	}
	var res []sc
	for _, c := range cands {
		d := lev(strings.ToLower(s), strings.ToLower(c))
		if d <= max(2, len(s)/4) || strings.Contains(strings.ToLower(c), strings.ToLower(s)) {
			res = append(res, sc{c, d})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].d < res[j].d })
	var out []string
	for i := 0; i < len(res) && i < 3; i++ {
		out = append(out, res[i].c)
	}
	return out
}

func lev(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev = cur
	}
	return prev[len(b)]
}

var _ = os.Getpid
