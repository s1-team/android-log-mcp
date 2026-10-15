package mcpserver

import (
	"context"
	"errors"
	"fmt"
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
	if !t.M.Lazy { // lazy: target already waited (Track)
		t.M.WaitReady(3 * time.Second)
	}
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
			return nil, errc("unknown_device", []string{"logcat_status"}, "value", serial, "reason", t.noDeviceHint())
		}
		var ids, next []string
		for _, d := range devs {
			ids = append(ids, d.Serial)
			next = append(next, param("device", d.Serial))
		}
		return nil, errc("unknown_device", next, "value", serial, "devices", strings.Join(ids, ","))
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
		var ids []string
		for _, d := range cand {
			ids = append(ids, d.Serial)
		}
		return nil, severalErr(ids)
	}
	for s, st := range live {
		if st != "device" {
			return nil, stateError(s, st)
		}
	}
	return nil, errc("no_device", []string{"logcat_status"}, "reason", t.noDeviceHint())
}

func stateError(serial, st string) error {
	switch st {
	case "unauthorized":
		return errc("device_unauthorized", []string{"accept the Allow USB debugging prompt on the device", "logcat_status"}, "device", serial)
	case "offline":
		return errc("device_offline", []string{"adb -s " + serial + " reconnect offline", "logcat_status"}, "device", serial)
	}
	return errc("device_state", []string{"logcat_status"}, "device", serial, "adb_state", st)
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
		prev = append(prev, fmt.Sprintf("%s (last seen at host time %s)", d.Serial, hostTime(time.Unix(0, m.LastSeenHostNs))))
	}
	if len(prev) > 0 {
		msg += " Previously recorded devices: " + strings.Join(prev, ", ") + " — pass device=\"<serial>\" to query stored logs."
	}
	return msg
}

func (t *Tools) status(ctx context.Context, a statusArgs) (string, error) {
	// record_for is validated before anything starts
	var pinFor time.Duration
	if a.RecordFor != "" {
		if t.Pins == nil || !t.M.Lazy {
			return "", errc("record_for_unavailable", []string{"logcat_status"}, "reason", "no_background_service")
		}
		d, err := ParsePin(a.RecordFor)
		if err != nil {
			return "", errc("bad_param", []string{`record_for="2h"`, `record_for="0"`}, "param", "record_for", "value", a.RecordFor, "valid", "30m|2h|1h30m|0 (max 24h)")
		}
		pinFor = d
	}
	// the device this call is about: started (a log-tool call), unless
	// several are online and none is named (then none starts)
	var (
		target   *device.Device
		prog     device.Progress
		several  *errSeveral
		useErr   error
		pinNote  string
		stopped  string
		targetID string
	)
	if a.RecordFor == "0" {
		id, sd, err := t.target(a.Device)
		switch {
		case err != nil:
			return "", err
		case id == "" && sd != nil:
			id = sd.Serial
		}
		if err := t.Pins.Stop(id); err != nil {
			return "", errf("%v", err)
		}
		t.M.Unwant(id, "stopped by logcat_status(record_for=\"0\")")
		stopped = id
	} else {
		start := time.Now()
		d, done, starting, err := t.use(a.Device, start)
		defer done()
		switch {
		case errors.As(err, &several):
			if a.RecordFor != "" || a.Package != "" {
				return "", err
			}
		case err != nil:
			useErr = err
			if a.RecordFor != "" || a.Package != "" {
				return "", err
			}
		default:
			target = d
			if t.M.Lazy && t.M.Wanted(d.Serial) {
				targetID = d.Serial
			}
		}
		if pinFor > 0 {
			if targetID == "" {
				return "", errc("device_offline", []string{"logcat_status"}, "device", d.Serial, "reason", "record_for_needs_connected_device")
			}
			until, err := t.Pins.Set(targetID, pinFor, "logcat_status(record_for)")
			if err != nil {
				return "", errf("%v", err)
			}
			pinNote = hdr("pinned", "device", targetID, "until", isoSec(until.Add(time.Duration(target.Store.Meta().ClockOffsetNs)).In(target.Loc())),
				"in_s", int(time.Until(until).Round(time.Second).Seconds())) + nextClause(`record_for="0"`)
		}
		if target != nil {
			if starting {
				prog = device.StartingProgress()
			} else {
				prog = target.WaitCaughtUp(ctx, t.waitLeft(start))
			}
		}
	}
	var head []StatusLine
	if t.Service != nil {
		head = append(head, t.Service()...)
	}
	if t.M.NoADB {
		head = append(head, StatusLine{Text: hdr("adb", "state", "not_used", "reason", "offline"), Label: "adb", Static: true})
	} else if err := t.M.ADBError(); err != nil {
		head = append(head, StatusLine{Text: hdr("hint", "code", "adb_not_found", "msg", err.Error()) + nextClause("adb version")})
	} else if adb := t.M.ADB(); adb != nil && (!t.M.Lazy || t.M.Tracking()) {
		v, err := adb.Version(ctx)
		if err != nil {
			head = append(head, StatusLine{Text: hdr("hint", "code", "adb_error", "msg", err.Error()) + nextClause("adb version")})
		} else {
			head = append(head, StatusLine{Text: hdr("adb", "state", "ok", "path", adb.Path, "version", v), Label: "adb", Static: true})
		}
		if werr := t.M.WatchError(); werr != nil {
			head = append(head, StatusLine{Text: hdr("hint", "code", "adb_server", "msg", werr.Error()) + nextClause("adb start-server")})
		}
	} else if adb != nil {
		head = append(head, StatusLine{Text: hdr("adb", "state", "idle", "path", adb.Path, "reason", "no_device_recorded"), Label: "adb", Static: true})
	}
	if stopped != "" {
		head = append(head, StatusLine{Text: hdr("stopped", "device", stopped, "pin", "removed") + nextClause(param("device", stopped))})
	}
	if pinNote != "" {
		head = append(head, StatusLine{Text: pinNote})
	}
	devs := t.M.Devices()
	live := t.M.LiveStates()
	if a.Device != "" {
		d := target
		if d == nil {
			d = t.M.Get(a.Device)
		}
		if d == nil {
			if useErr != nil {
				return "", useErr
			}
			_, err := t.resolveDevice(a.Device)
			return "", err
		}
		devs = []*device.Device{d}
	}
	head = append(head, StatusLine{Text: hdr("store", "path", t.M.Base, "data", filepath.Base(store.DataDir(t.M.Base))+"/<device-id>"), Label: "store", Static: true})
	if t.M.NoADB {
		head = append(head, StatusLine{Text: hdr("mode", "offline", 1, "read_only", 1)})
	}
	var body strings.Builder
	renderStatusHead(&body, head, statusMemoFrom(ctx), a.Verbose)
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
			state = "not_connected"
		}
		kv := []any{"id", d.Serial}
		if st.State != "" && d.Addr() != d.Serial {
			kv = append(kv, "via", d.Addr())
		}
		kv = append(kv, "state", strings.ReplaceAll(state, " ", "_"), "model", orQ(m.Model))
		if m.Release != "" {
			kv = append(kv, "android", m.Release, "sdk", m.SDK)
		}
		kv = append(kv, "tz", d.TZName(), "tz_offset", time.Now().In(loc).Format("-07:00"))
		fmt.Fprintf(&body, "%s\n", hdr("device", kv...))
		stats := d.Store.Stats()
		rk, rnext := t.recKV(d, st)
		fmt.Fprintf(&body, "  %s%s\n", hdr("recording", rk...), nextClause(rnext...))
		if t.Pins != nil {
			if p := t.Pins.Describe(d.Serial, loc); p != "" {
				fmt.Fprintf(&body, "  %s\n", hdr("pin", "msg", p)+nextClause(`record_for="0"`))
			}
		}
		for _, n := range []struct{ code, msg string }{{"id", st.IDNote}, {"rollback", d.RollbackNote()}, {"recovered", d.RecoveredNote()}, {"handoff", d.HandoffNote()}} {
			if n.msg != "" {
				fmt.Fprintf(&body, "  %s\n", hdr("note", "code", n.code, "msg", n.msg))
			}
		}
		d.RefreshAutoTime(context.Background()) // only when a clock change made it stale
		ci := d.Clock()
		if ci.AutoTime != "" {
			fmt.Fprintf(&body, "  %s\n", hdr("clock", "auto_time", ci.AutoTime, "source", orStr(ci.Source, "none")))
		}
		if js := d.Store.ClockJumps(); len(js) > 0 {
			j := js[len(js)-1]
			fmt.Fprintf(&body, "  clock_jumps stored=%d last_seq=%d%s\n", len(js), j.Seq, nextClause(`query="*"`))
			if a.Verbose {
				body.WriteString("  " + jumpLines(j, loc, -1, -1, ""))
			}
		}
		wk := []any{}
		if m.RecordingSince != 0 {
			wk = append(wk, "recording_since", isoSec(time.Unix(0, m.RecordingSince).Local()))
		}
		if stats.OldestNs != 0 {
			wk = append(wk, "oldest", isoNs(stats.OldestNs, loc), "newest", isoNs(stats.NewestNs, loc))
		}
		if len(wk) > 0 {
			fmt.Fprintf(&body, "  %s\n", hdr("window", wk...))
		}
		cfg := d.Store.Config()
		sk := []any{"lines", int(stats.Records), "bytes", int(stats.Bytes), "retention", store.FormatRetention(cfg.MaxAge), "max_mib", int(cfg.MaxBytes >> 20)}
		if len(m.Gaps) > 0 {
			sk = append(sk, "gaps", len(m.Gaps), "latest_gap", gapText(m.Gaps[len(m.Gaps)-1], loc))
		}
		fmt.Fprintf(&body, "  %s\n", hdr("stored", sk...))
		if stats.MemLoaded {
			fmt.Fprintf(&body, "  %s\n", hdr("memory", "window_lines", stats.MemRecords, "window_bytes", int(stats.MemBytes), "cache_bytes", int(stats.CacheBytes)))
		} else {
			fmt.Fprintf(&body, "  %s\n", hdr("memory", "window_loaded", 0, "cache_bytes", int(stats.CacheBytes)))
		}
		if note := lagNote(d, st, m, stats); note != "" && st.Recording {
			fmt.Fprintf(&body, "  %s\n", hdr("note", "code", "lag", "msg", note))
		}
		if hostLoc := time.Local; st.State == "device" {
			_, ho := time.Now().In(hostLoc).Zone()
			_, do := time.Now().In(loc).Zone()
			if ho != do {
				fmt.Fprintf(&body, "  %s\n", hdr("note", "code", "host_tz_differs", "host_tz", hostLoc.String(), "host_tz_offset", time.Now().In(hostLoc).Format("-07:00")))
			}
		}
	}
	liveIDs := make([]string, 0, len(live))
	for s := range live {
		liveIDs = append(liveIDs, s)
	}
	sort.Strings(liveIDs)
	for _, s := range liveIDs {
		stt := live[s]
		if known[s] || (a.Device != "" && a.Device != s) {
			continue
		}
		var reason string
		var next []string
		switch {
		case stt == "unauthorized":
			reason, next = "unauthorized", []string{"accept the Allow USB debugging prompt on the device"}
		case stt == "device" && t.M.AddrOf(s) == "":
			reason, next = "identifying", []string{"logcat_status"}
		case stt == "device" && t.M.Lazy:
			reason, next = "not_recorded", []string{param("device", t.M.AddrOf(s))}
		case stt == "device":
			reason, next = "device_limit", nil
		default:
			reason, next = "adb_state_not_ready", []string{"logcat_status"}
		}
		kv := []any{"id", s, "state", stt, "recorded", 0, "reason", reason}
		if reason == "device_limit" {
			kv = append(kv, "max", t.M.MaxDevices)
		}
		fmt.Fprintf(&body, "%s\n", hdr("device", kv...)+nextClause(next...))
	}
	if len(devs) == 0 && len(live) == 0 {
		body.WriteString(hdr("hint", "code", "no_device", "msg", t.noDeviceHint()) + nextClause("adb devices") + "\n")
	}
	if useErr != nil && several == nil {
		var te *toolError
		if errors.As(useErr, &te) {
			if !(te.code == "no_device" && len(devs) == 0 && len(live) == 0) { // the hint line says it
				body.WriteString(hdr("note", append([]any{"code", te.code}, te.kv...)...) + nextClause(te.next...) + "\n")
			}
		} else {
			body.WriteString(hdr("note", "code", "failed", "msg", useErr.Error()) + "\n")
		}
	}
	// cursor + package for the resolved device
	word := "ok"
	if len(devs) == 0 && len(live) == 0 {
		word = "empty"
	}
	hk := []any{"view", "status", "devices", len(devs)}
	var tail strings.Builder
	switch {
	case several != nil:
		hk = append(hk, "device", "none", "cursor", "none")
		var ids, next []string
		for _, i := range several.ids {
			ids = append(ids, i.ID)
			next = append(next, param("device", i.ID))
		}
		tail.WriteString(hdr("hint", "code", "several_devices", "devices", strings.Join(ids, ","), "started", 0) + nextClause(next...) + "\n")
	case target == nil:
		hk = append(hk, "device", "none", "cursor", "none")
	default:
		d := target
		hk = append(hk, "device", d.Serial)
		if prog.Pending {
			hk = append(hk, "caught_up", 0, "cursor", "none")
			tail.WriteString(hdr("note", append(append([]any{"code", "backfill"}, backfillKV(prog, d.Loc())...), "results", "incomplete")...) + nextClause("logcat_status") + "\n")
		} else {
			m := d.Store.Meta()
			boot := "0"
			if len(m.Sessions) > 0 {
				boot = m.Sessions[len(m.Sessions)-1].Boot
			}
			cur := store.Cursor{Serial: d.Serial, Boot: boot, Seq: d.Store.LastSeq()}
			hk = append(hk, "caught_up", 1, "cursor", cur.String())
			if prog.Note != "" {
				tail.WriteString(hdr("note", "code", "catchup", "msg", prog.Note) + "\n")
			}
		}
		if a.Verbose {
			hk = append(hk, "tz", d.TZName())
		}
		hk = append(hk, clockKV(d.Clock(), a.Verbose)...)
		if a.Package != "" {
			tail.WriteString(t.packageStatus(d, a.Package, a.Verbose))
		}
	}
	hk = append(hk, "fmt", fmtVersion)
	return hdr(word, hk...) + "\n" + body.String() + tail.String(), nil
}

// recKV is a device's recording state: key=values and the executable next.
func (t *Tools) recKV(d *device.Device, st device.Status) (kv []any, next []string) {
	lazy := t.M.Lazy && !t.M.NoADB
	otherWriter := func() []any {
		kv := []any{"state", "not_recorded", "reason", "other_writer"}
		if st.WriterPID != "" {
			kv = append(kv, "writer_pid", st.WriterPID)
		}
		return kv
	}
	switch {
	case st.Paused != "":
		kv = []any{"state", "paused", "msg", st.Paused}
	case st.Recording && lazy:
		kv = []any{"state", "recording"}
		if p := d.CatchUp(); p.Pending {
			kv = append(kv, "caught_up", 0)
			kv = append(kv, backfillKV(p, d.Loc())...)
		} else {
			kv = append(kv, "caught_up", 1)
		}
		switch idle := t.M.IdleIn(d.Serial, time.Now()); {
		case idle < 0:
			kv = append(kv, "idle_stop", "never")
		case t.M.LastCall(d.Serial).IsZero():
			kv = append(kv, "idle_stop", "pinned")
		default:
			kv = append(kv, "idle_stop_s", int(idle.Round(time.Second).Seconds()))
			next = []string{`record_for="2h"`}
		}
	case st.Recording:
		kv = []any{"state", "recording"}
	case lazy && st.State == "device" && t.M.Wanted(d.Serial) && !st.Writer && st.Owned:
		kv = otherWriter()
	case lazy && st.State == "device" && t.M.Wanted(d.Serial):
		kv = []any{"state", "starting"}
	case lazy && st.State == "device":
		kv, next = []any{"state", "not_recorded", "reason", "not_started"}, []string{param("device", d.Serial)}
	case lazy && t.M.Wanted(d.Serial) && t.M.Settling():
		kv = []any{"state", "starting", "reason", "waiting_for_adb", "msg", "waiting for adb to list the device and read its identity; recording begins as soon as it is online"}
	case lazy && t.M.Wanted(d.Serial):
		kv = []any{"state", "not_connected", "reason", "resumes_on_reconnect"}
	case st.State == "device" && !st.Owned && !t.M.NoADB:
		kv = []any{"state", "not_recorded", "reason", "device_limit", "max", t.M.MaxDevices}
	case st.State == "device" && !st.Writer:
		kv = otherWriter()
	case st.State == "device":
		kv = []any{"state", "starting"}
	default:
		kv = []any{"state", "not_connected"}
	}
	if st.LastErr != "" {
		kv = append(kv, "last_error", st.LastErr)
	}
	return kv, next
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
		to = isoNs(g.ToNs, loc)
	}
	from := "?"
	if g.FromNs > 0 {
		from = isoNs(g.FromNs, loc)
	}
	return fmt.Sprintf("%s..%s %s", from, to, noSpace(g.Reason))
}

func (t *Tools) packageStatus(d *device.Device, pkg string, verbose bool) string {
	var b strings.Builder
	loc := d.Loc()
	pk := d.Packages()
	installed, uid := "unknown", -1
	if len(pk) > 0 {
		installed = "0"
		for u, l := range pk {
			for _, p := range l {
				if p == pkg {
					installed, uid = "1", u
				}
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
	own := procs.NameOwner(pk) // dotted process names (issue #41)
	ps := d.Procs()
	su := newSharedUID(pkg, pk, func() []procs.Proc { return ps }) // shared-uid processes (issue #44)
	for _, p := range ps {
		// app processes only: helpers running with the app's uid
		// (crash_dump64, sh …) have no package and are not listed
		if !(p.Package == pkg || own(p.Name) == pkg || su.process(p.Name)) {
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
	kv := []any{"id", pkg, "installed", installed}
	if uid >= 0 {
		kv = append(kv, "uid", uid)
	}
	kv = append(kv, "running", len(alive))
	if len(alive) == 0 && len(dead) > 0 {
		kv = append(kv, "last_pid", dead[0].PID, "ended", isoNs(dead[0].EndNs, loc))
	}
	if !connected && len(alive) > 0 {
		kv = append(kv, "device_connected", 0, "running_as_of", "last_seen")
	}
	if su != nil {
		kv = append(kv, "shares_uid", 1)
	}
	b.WriteString(hdr("package", kv...) + "\n")
	if su != nil {
		if l := su.noteLine(verbose); l != "" {
			b.WriteString(l)
			if !strings.HasSuffix(l, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	if installed == "unknown" {
		b.WriteString(hdr("hint", "code", "package_list_pending") + nextClause(`package="`+pkg+`"`) + "\n")
	}
	if installed == "0" {
		if s := similar(pkg, allPkgs(pk)); len(s) > 0 {
			var next []string
			for _, p := range s {
				next = append(next, param("package", p))
			}
			b.WriteString(hdr("hint", "code", "package_not_installed", "similar", strings.Join(s, ",")) + nextClause(next...) + "\n")
		} else {
			b.WriteString(hdr("hint", "code", "package_not_installed") + nextClause("adb shell pm list packages") + "\n")
		}
	}
	crashes := d.Crashes()
	crashOf := func(p procs.Proc) []any {
		for _, c := range crashes {
			if c.PID == p.PID && c.Boot == p.Boot && c.TimeNs >= startOf(p)-5e9 && (p.EndNs <= 0 || c.TimeNs <= p.EndNs+5e9) {
				if c.Kind == "anr" {
					return []any{"anr", c.ID}
				}
				return []any{"crash", c.ID}
			}
		}
		return nil
	}
	for _, p := range alive {
		k := []any{"pid", p.PID, "name", procLabelName(p.Name), "state", "running"}
		switch {
		case p.StartNs > 0:
			k = append(k, "since", isoNs(p.StartNs, loc))
		case p.SeenNs > 0:
			k = append(k, "since_at_least", isoNs(p.SeenNs, loc))
		default:
			k = append(k, "since", "before_recording")
		}
		b.WriteString("  " + hdr("proc", k...) + "\n")
	}
	ended := func(p procs.Proc, scope string) []any {
		k := []any{"pid", p.PID, "name", procLabelName(p.Name), "state", "ended"}
		switch {
		case p.StartNs > 0:
			k = append(k, "start", isoNs(p.StartNs, loc))
		case p.SeenNs > 0:
			k = append(k, "start_at_most", isoNs(p.SeenNs, loc))
		default:
			k = append(k, "start", "before_recording")
		}
		if p.EndNs > 0 {
			k = append(k, "end", isoNs(p.EndNs, loc))
		} else {
			k = append(k, "end", "?")
		}
		k = append(k, "reason", noSpace(orStr(p.EndReason, "ended")))
		k = append(k, crashOf(p)...)
		return append(k, "scope", scope)
	}
	capN := 6
	if verbose {
		capN = 1 << 30
	}
	for i, p := range dead {
		if i == capN {
			b.WriteString(truncLine("procs_ended", len(dead)-capN, true, nil, "verbose=true"))
			break
		}
		b.WriteString("  " + hdr("proc", ended(p, "this_boot")...) + "\n")
	}
	for i, p := range prior {
		if i == capN {
			b.WriteString(truncLine("procs_before_reboot", len(prior)-capN, true, nil, "verbose=true"))
			break
		}
		if p.Alive() { // never closed (older store): the reboot ended it
			p.EndReason = "device rebooted"
		}
		b.WriteString("  " + hdr("proc", append(ended(p, "before_reboot"), "boot", p.Boot)...) + "\n")
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

// procLabelName is a process name as one token.
func procLabelName(n string) string {
	if n == "" {
		return "?"
	}
	return noSpace(n)
}

// statusClock: device now, host now, clock offset, auto_time and the
// clock jumps stored (issue #48 D4).
