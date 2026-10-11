package perf

import (
	"fmt"
	"math"
)

// Rules are the regression thresholds. A metric regresses when it is worse
// than the baseline by more than BOTH its relative and its absolute
// tolerance (the absolute one keeps tiny values from flapping).
type Rules struct {
	ExactRel, ExactAbs           float64 // output / disk bytes
	AllocsRel, AllocsAbs         float64 // allocs/op
	AllocBytesRel, AllocBytesAbs float64 // B/op
	TimeFactor, TimeAbsMs        float64 // latency, after calibration
	RateFactor                   float64 // throughput, after calibration
	MemRel, MemAbsMiB            float64
}

// DefaultRules: deterministic metrics strictly, timing generously.
var DefaultRules = Rules{
	ExactRel: 0.02, ExactAbs: 16,
	AllocsRel: 0.10, AllocsAbs: 2,
	AllocBytesRel: 0.15, AllocBytesAbs: 512,
	TimeFactor: 2.5, TimeAbsMs: 5,
	RateFactor: 2.5,
	MemRel:     0.5, MemAbsMiB: 16,
}

// Finding is one comparison result worth showing.
type Finding struct {
	Name      string
	Base, Cur float64
	Unit      string
	Fail      bool   // a regression (or a broken budget, or a missing metric)
	Better    bool   // clearly improved: consider re-recording the baseline
	Why       string // the rule that triggered
}

func (f Finding) String() string {
	tag := "ok"
	switch {
	case f.Fail:
		tag = "REGRESSION"
	case f.Better:
		tag = "improved"
	}
	return fmt.Sprintf("%-10s %-44s base %s → %s %s  (%s)", tag, f.Name, Fmt(f.Base), Fmt(f.Cur), f.Unit, f.Why)
}

// Fmt prints a value compactly.
func Fmt(v float64) string {
	switch {
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return fmt.Sprintf("%.0f", v)
	case math.Abs(v) >= 100:
		return fmt.Sprintf("%.0f", v)
	case math.Abs(v) >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// Scale is the machine speed factor cur/base from the calibration (1 if
// either is missing). Timing baselines are multiplied by it.
func Scale(base, cur *Results) float64 {
	b, c := base.Metrics[CalibKey].Value, cur.Metrics[CalibKey].Value
	if b <= 0 || c <= 0 {
		return 1
	}
	return c / b
}

// Compare checks cur against base. Every metric of the baseline must be
// present in cur. Budgets are checked on cur alone.
func Compare(base, cur *Results, r Rules) []Finding {
	scale := Scale(base, cur)
	var out []Finding
	for _, name := range base.Names() {
		b := base.Metrics[name]
		c, ok := cur.Metrics[name]
		if !ok {
			out = append(out, Finding{Name: name, Base: b.Value, Unit: b.Unit, Fail: true, Why: "metric missing from this run"})
			continue
		}
		f := Finding{Name: name, Base: b.Value, Cur: c.Value, Unit: c.Unit}
		worse := func(rel, abs float64) bool { // lower is better
			return c.Value > b.Value*(1+rel) && c.Value-b.Value > abs
		}
		better := func(rel, abs float64) bool {
			return c.Value < b.Value*(1-rel) && b.Value-c.Value > abs
		}
		switch c.Kind {
		case Exact:
			f.Fail, f.Better = worse(r.ExactRel, r.ExactAbs), better(r.ExactRel, r.ExactAbs)
			f.Why = fmt.Sprintf("exact: > +%.0f%% and > +%s", r.ExactRel*100, Fmt(r.ExactAbs))
		case Allocs:
			rel, abs := r.AllocsRel, r.AllocsAbs
			if c.Unit == "B/op" {
				rel, abs = r.AllocBytesRel, r.AllocBytesAbs
			}
			f.Fail, f.Better = worse(rel, abs), better(rel, abs)
			f.Why = fmt.Sprintf("allocs: > +%.0f%% and > +%s", rel*100, Fmt(abs))
		case Time:
			exp := b.Value * scale
			f.Fail = c.Value > exp*r.TimeFactor && c.Value-exp > r.TimeAbsMs
			f.Better = c.Value < exp/r.TimeFactor && exp-c.Value > r.TimeAbsMs
			f.Why = fmt.Sprintf("time: > %.1f× calibrated base %s and > +%sms", r.TimeFactor, Fmt(exp), Fmt(r.TimeAbsMs))
		case Rate:
			exp := b.Value / scale
			f.Fail = c.Value < exp/r.RateFactor
			f.Better = c.Value > exp*r.RateFactor
			f.Why = fmt.Sprintf("rate: < calibrated base %s / %.1f", Fmt(exp), r.RateFactor)
		case Mem:
			f.Fail, f.Better = worse(r.MemRel, r.MemAbsMiB), better(r.MemRel, r.MemAbsMiB)
			f.Why = fmt.Sprintf("mem: > +%.0f%% and > +%s MiB", r.MemRel*100, Fmt(r.MemAbsMiB))
		default:
			f.Why = "info"
		}
		if c.Budget > 0 {
			over := c.Value > c.Budget
			if c.Kind == Rate {
				over = c.Value < c.Budget
			}
			if over {
				f.Fail = true
				f.Why = fmt.Sprintf("budget %s %s broken", Fmt(c.Budget), c.Unit)
			}
		}
		out = append(out, f)
	}
	return out
}

// Failed reports whether any finding is a failure.
func Failed(fs []Finding) bool {
	for _, f := range fs {
		if f.Fail {
			return true
		}
	}
	return false
}
