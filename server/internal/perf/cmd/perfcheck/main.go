// Command perfcheck merges the result files written by the perf tests,
// compares them with the baseline and fails on a regression, or records a
// new baseline (issue #13; docs/performance.md). Run it through
// scripts/perf.sh.
//
//	perfcheck -baseline ../perf/baseline.json [-update] [-md table.md] [-out merged.json] results/*.json
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/perf"
)

func main() {
	basePath := flag.String("baseline", "../perf/baseline.json", "baseline file")
	update := flag.Bool("update", false, "write the merged results as the new baseline instead of comparing")
	md := flag.String("md", "", "also write a markdown table of all metrics to this file")
	out := flag.String("out", "", "also write the merged results to this file")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: perfcheck [-baseline F] [-update] [-md F] [-out F] RESULT.json...")
		os.Exit(2)
	}
	cur, err := merge(flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfcheck:", err)
		os.Exit(2)
	}
	if *out != "" {
		must(cur.Save(*out))
	}
	if *md != "" {
		must(os.WriteFile(*md, []byte(Table(cur)), 0o644))
	}
	if *update {
		must(cur.Save(*basePath))
		fmt.Printf("baseline written: %s (%d metrics, %s %s, %s)\n", *basePath, len(cur.Metrics), cur.Machine.GOOS, cur.Machine.GOARCH, cur.Machine.CPU)
		return
	}
	base, err := perf.Load(*basePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfcheck:", err)
		os.Exit(2)
	}
	fs := perf.Compare(base, cur, perf.DefaultRules)
	fmt.Printf("baseline: %s (%s, %s); this run: %s (%s); machine speed factor %.2f (calibration %.1f → %.1f ms)\n",
		*basePath, base.Machine.Date, orDash(base.Machine.CPU), cur.Machine.Date, orDash(cur.Machine.CPU),
		perf.Scale(base, cur), base.Metrics[perf.CalibKey].Value, cur.Metrics[perf.CalibKey].Value)
	sort.SliceStable(fs, func(i, j int) bool { return rank(fs[i]) < rank(fs[j]) })
	var fails, better int
	for _, f := range fs {
		if f.Fail {
			fails++
		}
		if f.Better {
			better++
		}
		fmt.Println(f)
	}
	for _, n := range cur.Names() {
		if _, ok := base.Metrics[n]; !ok {
			fmt.Printf("%-10s %-44s %s %s (not in the baseline; re-record it to start guarding this metric)\n", "new", n, perf.Fmt(cur.Metrics[n].Value), cur.Metrics[n].Unit)
		}
	}
	fmt.Printf("\n%d metrics compared: %d regression(s), %d clearly improved\n", len(fs), fails, better)
	if better > 0 && fails == 0 {
		fmt.Println("improvements: if intended, re-record the baseline with scripts/perf.sh --update (docs/performance.md)")
	}
	if perf.Failed(fs) {
		fmt.Println("FAIL: performance regression (see docs/performance.md for the rules and how to update the baseline when a change is intended)")
		os.Exit(1)
	}
}

func rank(f perf.Finding) int {
	switch {
	case f.Fail:
		return 0
	case f.Better:
		return 1
	}
	return 2
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// merge reads the result files; the calibration is averaged over them
// (each test binary measures it), other metrics must not collide.
func merge(paths []string) (*perf.Results, error) {
	cur := perf.New()
	var calib []float64
	for _, p := range paths {
		r, err := perf.Load(p)
		if err != nil {
			return nil, err
		}
		if c, ok := r.Metrics[perf.CalibKey]; ok {
			calib = append(calib, c.Value)
			delete(r.Metrics, perf.CalibKey)
		}
		for k := range r.Metrics {
			if _, dup := cur.Metrics[k]; dup {
				return nil, fmt.Errorf("metric %s in more than one result file", k)
			}
		}
		cur.Merge(r)
		cur.Machine = r.Machine
	}
	if len(calib) > 0 {
		s := 0.0
		for _, c := range calib {
			s += c
		}
		cur.Set(perf.CalibKey, perf.Metric{Value: float64(int(s/float64(len(calib))*100)) / 100, Unit: "ms", Kind: perf.Info,
			Desc: "stdlib-only CPU calibration workload (mean over the test binaries)"})
	}
	return cur, nil
}

// Table renders all metrics as markdown.
func Table(r *perf.Results) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Recorded %s on %s/%s, %s, %d CPUs, %s.\n\n", r.Machine.Date, r.Machine.GOOS, r.Machine.GOARCH, orDash(r.Machine.CPU), r.Machine.NumCPU, r.Machine.GoVersion)
	b.WriteString("| metric | value | unit | kind | budget | what |\n|---|---:|---|---|---:|---|\n")
	for _, n := range r.Names() {
		m := r.Metrics[n]
		budget := ""
		if m.Budget > 0 {
			budget = perf.Fmt(m.Budget)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n", n, perf.Fmt(m.Value), m.Unit, m.Kind, budget, strings.ReplaceAll(m.Desc, "|", "\\|"))
	}
	return b.String()
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfcheck:", err)
		os.Exit(2)
	}
}
