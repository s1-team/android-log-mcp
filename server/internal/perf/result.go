// Package perf is the performance baseline and regression guard (issue
// #13): result files, the comparison rules, the deterministic synthetic
// dataset and the CPU calibration. The measurements themselves are tests
// (perf_test.go here, cmd/android-log-mcp/perf_test.go for the processes)
// that run only with ALM_PERF_OUT set; scripts/perf.sh runs them and
// cmd/perfcheck compares the result with perf/baseline.json. See
// docs/performance.md.
package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// EnvOut names the directory the perf tests write their result files to;
// without it they skip.
const EnvOut = "ALM_PERF_OUT"

// Kind decides how a metric is compared (see Compare).
type Kind string

const (
	Exact  Kind = "exact"  // deterministic size (output bytes, disk bytes): strict
	Allocs Kind = "allocs" // allocations per op / bytes allocated per op: tight
	Time   Kind = "time"   // latency, lower is better: generous, calibrated
	Rate   Kind = "rate"   // throughput, higher is better: generous, calibrated
	Mem    Kind = "mem"    // resident / heap memory: loose
	Info   Kind = "info"   // context only, never fails
)

// Metric is one measured value.
type Metric struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	Kind  Kind    `json:"kind"`
	// Budget, if set, is an absolute ceiling (for Rate: floor) that holds
	// on any machine, whatever the baseline says: the product targets
	// (docs/design-v3.md §6, docs/performance.md).
	Budget float64 `json:"budget,omitempty"`
	Desc   string  `json:"desc,omitempty"`
}

// Machine describes where results were recorded.
type Machine struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	GoVersion string `json:"go"`
	CPU       string `json:"cpu,omitempty"`
	NumCPU    int    `json:"num_cpu"`
	Host      string `json:"host,omitempty"`
	Date      string `json:"date"`
	Commit    string `json:"commit,omitempty"`
}

// Results is a result file (and the baseline).
type Results struct {
	Schema  int               `json:"schema"`
	Machine Machine           `json:"machine"`
	Metrics map[string]Metric `json:"metrics"`
}

// CalibKey is the calibration metric (see Calibrate).
const CalibKey = "calib.cpu_ms"

// New returns empty results describing this machine.
func New() *Results {
	return &Results{Schema: 1, Machine: ThisMachine(), Metrics: map[string]Metric{}}
}

// ThisMachine describes the current machine.
func ThisMachine() Machine {
	return Machine{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), CPU: cpuModel(),
		NumCPU: runtime.NumCPU(), Host: os.Getenv("ALM_PERF_HOST"), Date: time.Now().UTC().Format(time.RFC3339),
		Commit: os.Getenv("ALM_PERF_COMMIT")}
}

func cpuModel() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Set records a metric.
func (r *Results) Set(name string, m Metric) { r.Metrics[name] = m }

// Load reads a result file.
func Load(path string) (*Results, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Results{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if r.Metrics == nil {
		r.Metrics = map[string]Metric{}
	}
	return r, nil
}

// Save writes r as indented JSON with sorted keys.
func (r *Results) Save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Merge adds the metrics of o (later files win).
func (r *Results) Merge(o *Results) {
	for k, v := range o.Metrics {
		r.Metrics[k] = v
	}
}

// Names returns the metric names, sorted.
func (r *Results) Names() []string {
	var out []string
	for k := range r.Metrics {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
