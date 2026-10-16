package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// logcat_app_launches across device clock jumps (issue #54): a synthetic
// store (arrival order, clock jumps in meta) queried through the tool,
// compared with the analysis of a full scan and with the known launches.

type lline struct {
	at  time.Time
	tag string
	msg string
}

type wantLaunch struct {
	req    time.Time
	ttid   int  // 0: not logged
	jumped bool // Displayed logged after a clock jump
}

type launchJumpFx struct {
	tools *Tools
	recs  []*model.Record
	steps []analysis.ClockStep
	want  []wantLaunch // arrival order
}

func newLaunchJumpFx(t *testing.T, devNow time.Time, segs [][]lline, want []wantLaunch) *launchJumpFx {
	t.Helper()
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 64 << 10, BlockBytes: 4 << 10, MemRecords: 200}
	st, err := store.Open(base, "emu-l", cfg)
	if err != nil {
		t.Fatal(err)
	}
	hostNow := time.Now()
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ = "emu-l", "UTC"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
		m.ClockOffsetNs, m.ClockHostNs = int64(devNow.Sub(hostNow)), hostNow.UnixNano()
	})
	fx := &launchJumpFx{want: want}
	var jumps []store.ClockJump
	for si, s := range segs {
		var recs []*model.Record
		for _, x := range s {
			r := &model.Record{TimeNs: x.at.UnixNano(), PID: 4321, TID: 4321, Level: 'I', Tag: x.tag, Msg: x.msg, Boot: "b1", UID: 10123, Package: "com.foo", Process: "com.foo"}
			if x.tag == "ActivityTaskManager" || x.tag == "F" {
				r.PID, r.TID, r.UID, r.Package, r.Process = 1880, 1900, 1000, "", "system_server"
			}
			recs = append(recs, r)
		}
		if err := st.Append(recs); err != nil {
			t.Fatal(err)
		}
		if si > 0 {
			prev := fx.recs[len(fx.recs)-1]
			j := store.ClockJump{Seq: recs[0].Seq, DeltaNs: recs[0].TimeNs - prev.TimeNs, BeforeNs: prev.TimeNs, AfterNs: recs[0].TimeNs, HostNs: hostNow.UnixNano(), Source: "live"}
			jumps = append(jumps, j)
			fx.steps = append(fx.steps, analysis.ClockStep{Seq: j.Seq, DeltaNs: j.DeltaNs})
		}
		fx.recs = append(fx.recs, recs...)
	}
	for _, j := range jumps {
		st.AddClockJump(j)
	}
	st.Close()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	fx.tools = &Tools{M: m}
	return fx
}

func lStart(at time.Time, act string) lline {
	return lline{at, "ActivityTaskManager", "START u0 {act=android.intent.action.MAIN cmp=com.foo/." + act + "} from uid 2000"}
}
func lCreate(at time.Time, act string) lline {
	return lline{at, "wm_on_create_called", "[1,com.foo." + act + ",performCreate,186]"}
}
func lResume(at time.Time, act string) lline {
	return lline{at, "wm_on_resume_called", "[1,com.foo." + act + ",RESUME_ACTIVITY,6]"}
}
func lDisp(at time.Time, act string, ms int) lline {
	return lline{at, "ActivityTaskManager", fmt.Sprintf("Displayed com.foo/.%s for user 0: +%dms", act, ms)}
}

// filler: n unrelated lines every step from start
func filler(start time.Time, n int, step time.Duration) []lline {
	var out []lline
	for i := 0; i < n; i++ {
		out = append(out, lline{start.Add(time.Duration(i) * step), "F", fmt.Sprintf("filler %d", i)})
	}
	return out
}

// a complete launch at t: request, onCreate, onResume, Displayed after ms
func complete(t time.Time, act string, ms int) []lline {
	return []lline{lStart(t, act), lCreate(t.Add(200*time.Millisecond), act), lResume(t.Add(300*time.Millisecond), act), lDisp(t.Add(time.Duration(ms)*time.Millisecond), act, ms)}
}

// spanning: request, onCreate, onResume before the jump (Displayed comes
// in the next segment)
func spanning(t time.Time, act string) []lline {
	return []lline{lStart(t, act), lCreate(t.Add(200*time.Millisecond), act), lResume(t.Add(300*time.Millisecond), act)}
}

func cat(parts ...[]lline) []lline {
	var out []lline
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

const ms = time.Millisecond

// The timeline: back a few minutes, back to 1970, forward a year, back
// to just before device now. Each jump has a launch spanning it, and
// launches on both sides with device times that interleave.
func launchTimeline(t *testing.T) (*launchJumpFx, time.Time) {
	n := time.Now().UTC().Truncate(time.Second).Add(-time.Hour) // device now
	t0 := n.Add(-2 * time.Hour)
	epoch := time.Unix(1000, 0).UTC()
	y := n.Add(yearD)
	e := n.Add(-10 * time.Minute)
	segs := [][]lline{
		// A: t0 … t0+10m
		cat(filler(t0, 300, time.Second), complete(t0.Add(5*time.Minute), "Main", 812), filler(t0.Add(5*time.Minute+time.Second), 290, time.Second), spanning(t0.Add(10*time.Minute), "Main")),
		// B: back ~4 min to t0+6m (continuous t0+10m+0.5s)
		cat([]lline{lDisp(t0.Add(6*time.Minute), "Main", 900)}, filler(t0.Add(6*time.Minute+ms), 30, time.Second), complete(t0.Add(6*time.Minute+40*time.Second), "Second", 450), filler(t0.Add(6*time.Minute+41*time.Second), 60, time.Second), spanning(t0.Add(8*time.Minute), "Main")),
		// C: back to 1970
		cat([]lline{lDisp(epoch, "Main", 700)}, filler(epoch.Add(ms), 20, time.Second), complete(epoch.Add(30*time.Second), "Main", 600), spanning(epoch.Add(40*time.Second), "Main")),
		// D: forward to device now + 1 year
		cat([]lline{lDisp(y, "Main", 1100)}, filler(y.Add(ms), 10, time.Second), complete(y.Add(20*time.Second), "Main", 500), spanning(y.Add(30*time.Second), "Main")),
		// E: back to device now − 10 min
		cat([]lline{lDisp(e, "Main", 1300)}, complete(e.Add(10*time.Second), "Main", 400), filler(e.Add(11*time.Second), 20, time.Second)),
	}
	want := []wantLaunch{
		{t0.Add(5 * time.Minute), 812, false},
		{t0.Add(10 * time.Minute), 900, true},
		{t0.Add(6*time.Minute + 40*time.Second), 450, false},
		{t0.Add(8 * time.Minute), 700, true},
		{epoch.Add(30 * time.Second), 600, false},
		{epoch.Add(40 * time.Second), 1100, true},
		{y.Add(20 * time.Second), 500, false},
		{y.Add(30 * time.Second), 1300, true},
		{e.Add(10 * time.Second), 400, false},
	}
	return newLaunchJumpFx(t, n, segs, want), n
}

var reLaunchHead = regexp.MustCompile(`^#\d+ (\S+ \S+)  (\S+)  `)
var reTTID = regexp.MustCompile(`^   TTID \(time to initial display, system-measured\): (\d+) ms`)

type gotLaunch struct {
	req    string
	ttid   int
	jumped bool
	text   string
}

// parseLaunches: the launches of a logcat_app_launches answer, newest first.
func parseLaunches(out string) []gotLaunch {
	var ls []gotLaunch
	for _, ln := range strings.Split(out, "\n") {
		if m := reLaunchHead.FindStringSubmatch(ln); m != nil {
			ls = append(ls, gotLaunch{req: m[1]})
			continue
		}
		if len(ls) == 0 || !strings.HasPrefix(ln, "   ") {
			continue
		}
		l := &ls[len(ls)-1]
		l.text += ln + "\n"
		if m := reTTID.FindStringSubmatch(ln); m != nil {
			l.ttid, _ = strconv.Atoi(m[1])
		}
		if strings.Contains(ln, "duration: unreliable (device clock jumped)") {
			l.jumped = true
		}
	}
	return ls
}

func fmtReq(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000") }

func TestLaunchesAcrossClockJumps(t *testing.T) {
	t.Parallel()
	fx, n := launchTimeline(t)
	full := analysis.LaunchesAcross(fx.recs, "", fx.steps) // full scan, arrival order
	if len(full) != len(fx.want) {
		t.Fatalf("full scan: %d launches, want %d", len(full), len(fx.want))
	}
	for i, w := range fx.want {
		l := full[i]
		if l.RequestNs != w.req.UnixNano() || int(l.Displayed.TookMs) != w.ttid || l.ClockJumped != w.jumped {
			t.Errorf("full scan #%d: request %s TTID %d jumped %v; want %s %d %v", i, fmtReq(time.Unix(0, l.RequestNs)), l.Displayed.TookMs, l.ClockJumped, fmtReq(w.req), w.ttid, w.jumped)
		}
	}
	epoch := time.Unix(0, 0).UTC()
	for _, w := range [][2]time.Time{
		{epoch.Add(time.Second), n.Add(2 * yearD)},                                 // everything
		{n.Add(-2 * time.Hour), n.Add(-time.Hour)},                                 // segments A and B
		{n.Add(-2*time.Hour + 9*time.Minute), n.Add(-time.Hour)},                   // the request before the back jump only
		{n.Add(-2*time.Hour + 7*time.Minute), n.Add(-2*time.Hour + 9*time.Minute)}, // spans to 1970
		{epoch.Add(time.Second), epoch.Add(time.Hour)},                             // 1970 only (spans to +1 y)
		{n.Add(yearD), n.Add(yearD + time.Hour)},                                   // +1 y only (spans back)
		{n.Add(-15 * time.Minute), n.Add(time.Minute)},                             // after the last jump
	} {
		args := fmt.Sprintf(`{"from":%q,"to":%q,"limit":20}`, iso(w[0]), iso(w[1]))
		out := fx.call(t, args)
		got := parseLaunches(out)
		var exp []*analysis.Launch
		for _, l := range full {
			if l.RequestNs >= w[0].UnixNano() && l.RequestNs < w[1].UnixNano() {
				exp = append([]*analysis.Launch{l}, exp...) // newest (arrival) first
			}
		}
		if len(got) != len(exp) {
			t.Errorf("%s: %d launches, full scan %d:\n%s", args, len(got), len(exp), out)
			continue
		}
		for i, l := range exp {
			g := got[i]
			if g.req != fmtReq(time.Unix(0, l.RequestNs)) || g.ttid != int(l.Displayed.TookMs) || g.jumped != l.ClockJumped {
				t.Errorf("%s #%d: got %s TTID %d jumped %v; full scan %s %d %v\n%s", args, i+1, g.req, g.ttid, g.jumped,
					fmtReq(time.Unix(0, l.RequestNs)), l.Displayed.TookMs, l.ClockJumped, out)
			}
			if g.jumped && regexp.MustCompile(`TTID.*logged at \+\d+ ms\]`).MatchString(g.text) {
				t.Errorf("%s #%d: a duration across the jump is shown:\n%s", args, i+1, g.text)
			}
			// no duration from subtracting device times across a jump
			for _, m := range regexp.MustCompile(`\+(\d+) ms`).FindAllStringSubmatch(g.text, -1) {
				if v, _ := strconv.Atoi(m[1]); v > 60000 {
					t.Errorf("%s #%d: duration %s ms:\n%s", args, i+1, m[1], g.text)
				}
			}
		}
		if len(exp) > 0 && !strings.Contains(out, "clock_jump: ") && strings.Contains(out, "unreliable") {
			t.Errorf("%s: no clock_jump line:\n%s", args, out)
		}
	}
}

func (fx *launchJumpFx) call(t *testing.T, args string) string {
	t.Helper()
	out, isErr := fx.tools.Call(context.Background(), "logcat_app_launches", []byte(args))
	if isErr {
		t.Fatalf("logcat_app_launches %s: error: %s", args, out)
	}
	return out
}
