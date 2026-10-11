package analysis

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/model"
)

// Launch pairing across device clock jumps (issue #54).

type lrec struct {
	t   time.Time
	tag string
	msg string
}

func lstart(t time.Time) lrec {
	return lrec{t, "ActivityTaskManager", "START u0 {act=android.intent.action.MAIN cmp=com.foo/.Main} from uid 2000"}
}
func lcreate(t time.Time) lrec {
	return lrec{t, "wm_on_create_called", "[1,com.foo.Main,performCreate,186]"}
}
func lresume(t time.Time) lrec {
	return lrec{t, "wm_on_resume_called", "[1,com.foo.Main,RESUME_ACTIVITY,6]"}
}
func ldisp(t time.Time, ms int) lrec {
	return lrec{t, "ActivityTaskManager", fmt.Sprintf("Displayed com.foo/.Main for user 0: +%dms", ms)}
}

// jumpRecs numbers the records in arrival order and returns the clock
// jumps between consecutive segments.
func jumpRecs(segs ...[]lrec) ([]*model.Record, []ClockStep) {
	var out []*model.Record
	var js []ClockStep
	for si, s := range segs {
		for i, x := range s {
			r := &model.Record{Seq: uint64(len(out) + 1), TimeNs: x.t.UnixNano(), PID: 4321, TID: 4321, Level: 'I', Tag: x.tag, Msg: x.msg, Package: "com.foo", Process: "com.foo", UID: 10123}
			if x.tag == "ActivityTaskManager" {
				r.PID, r.Package, r.Process, r.UID = 1880, "", "system_server", 1000
			}
			if si > 0 && i == 0 {
				js = append(js, ClockStep{Seq: r.Seq, DeltaNs: r.TimeNs - out[len(out)-1].TimeNs})
			}
			out = append(out, r)
		}
	}
	return out, js
}

func TestLaunchesAcrossBackJumpNoCrossPairing(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 10, 20, 0, 0, time.UTC)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	// R1 at 10:20:00; the clock is set back 90 s; R1's Displayed is logged
	// at 10:18:31 (continuous 10:20:01). R2 is requested at 10:19:59 and
	// its Displayed is logged at 10:20:01: sorted by device time alone, R1
	// would take R2's Displayed and R2 none.
	recs, js := jumpRecs(
		[]lrec{lstart(t0), lcreate(t0.Add(ms(300))), lresume(t0.Add(ms(400)))},
		[]lrec{ldisp(t0.Add(-89*time.Second), 1000), lstart(t0.Add(-time.Second)), lcreate(t0.Add(-ms(500))), lresume(t0.Add(-ms(200))), ldisp(t0.Add(time.Second), 2000)},
	)
	if len(js) != 1 || js[0].DeltaNs > -60e9 {
		t.Fatalf("fixture jumps %+v", js)
	}
	ls := LaunchesAcross(recs, "com.foo", js)
	if len(ls) != 2 {
		t.Fatalf("%d launches", len(ls))
	}
	r1, r2 := ls[0], ls[1]
	if r1.RequestNs != t0.UnixNano() || r1.Displayed.TookMs != 1000 || !r1.Displayed.AfterJump || !r1.ClockJumped {
		t.Errorf("R1: %+v", r1)
	}
	if r1.OnResume.AfterJump || (r1.OnResume.Ns-r1.RequestNs)/1e6 != 400 || r1.OnCreate.TookMs != 186 {
		t.Errorf("R1 steps before the jump: %+v %+v", r1.OnCreate, r1.OnResume)
	}
	if r2.Displayed.TookMs != 2000 || r2.ClockJumped || r2.OnResume.Ns != t0.Add(-ms(200)).UnixNano() {
		t.Errorf("R2: %+v", r2)
	}
	// without the jump the device-time sort pairs across the timelines
	// (sorted by device time alone, R1 takes R2's Displayed: the #54 bug)
	for _, l := range Launches(recs, "com.foo") {
		if l.RequestNs == t0.UnixNano() && l.Displayed.TookMs != 2000 {
			t.Errorf("fixture no longer reproduces #54: R1 TTID %d without jumps", l.Displayed.TookMs)
		}
	}
}

func TestLaunchesAcrossJumps(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	year := 365 * 24 * time.Hour
	epoch := time.Unix(1000, 0).UTC()
	for _, c := range []struct {
		name string
		to   time.Time // device time after the jump
	}{
		{"back 5 min", now.Add(-5 * time.Minute)},
		{"back 3 h", now.Add(-3 * time.Hour)},
		{"back to 1970", epoch},
		{"forward 1 day", now.Add(24 * time.Hour)},
		{"forward 1 year", now.Add(year)},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A complete before the jump, B spans it (Displayed after),
			// C complete after it.
			recs, js := jumpRecs(
				[]lrec{lstart(now), lcreate(now.Add(300 * time.Millisecond)), lresume(now.Add(400 * time.Millisecond)), ldisp(now.Add(800*time.Millisecond), 812),
					lstart(now.Add(10 * time.Second)), lcreate(now.Add(10*time.Second + 200*time.Millisecond)), lresume(now.Add(10*time.Second + 300*time.Millisecond))},
				[]lrec{ldisp(c.to, 900), lstart(c.to.Add(5 * time.Second)), lresume(c.to.Add(5*time.Second + 100*time.Millisecond)), ldisp(c.to.Add(5*time.Second+300*time.Millisecond), 300)},
			)
			ls := LaunchesAcross(recs, "", js)
			if len(ls) != 3 {
				t.Fatalf("%d launches", len(ls))
			}
			a, b, cc := ls[0], ls[1], ls[2]
			if a.Displayed.TookMs != 812 || a.ClockJumped || (a.OnResume.Ns-a.RequestNs)/1e6 != 400 {
				t.Errorf("A: %+v", a)
			}
			if b.RequestNs != now.Add(10*time.Second).UnixNano() || b.Displayed.TookMs != 900 || !b.Displayed.AfterJump || !b.ClockJumped || b.OnResume.AfterJump {
				t.Errorf("B: %+v", b)
			}
			if cc.RequestNs != c.to.Add(5*time.Second).UnixNano() || cc.Displayed.TookMs != 300 || cc.ClockJumped || cc.Kind != "HOT" {
				t.Errorf("C: %+v", cc)
			}
		})
	}
}

// without jumps LaunchesAcross is Launches
func TestLaunchesAcrossNoJumps(t *testing.T) {
	a := Launches(records(t, launchLog), "com.foo")
	b := LaunchesAcross(records(t, launchLog), "com.foo", []ClockStep{{Seq: 1 << 40, DeltaNs: -1e12}})
	if !reflect.DeepEqual(a, b) {
		t.Errorf("differ:\n%+v\n%+v", *a[0], *b[0])
	}
}
