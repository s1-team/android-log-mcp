package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/fakeadb"
	"github.com/s1crab/android-log-mcp/server/internal/perf"
)

// Process-level performance (issue #13; docs/performance.md): the real
// binary, the background service it spawns and the scriptable fake adb, in
// an isolated cache dir. Runs only with ALM_PERF_OUT=<dir> (scripts/perf.sh)
// and writes <dir>/process.json.

// buildBinary builds the real CLI (not this test binary, whose size and
// start-up would not be representative).
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "android-log-mcp")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

type mcpProc struct {
	cs  *mcp.ClientSession
	cmd *exec.Cmd
}

// startReal starts the binary as an MCP server and returns once
// initialize and tools/list are answered, with the time that took.
func startReal(t *testing.T, bin string, env []string) (*mcpProc, time.Duration) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	client := mcp.NewClient(&mcp.Implementation{Name: "perf", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t0 := time.Now()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 10 * time.Second}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	el := time.Since(t0)
	p := &mcpProc{cs: cs, cmd: cmd}
	t.Cleanup(func() { cs.Close() })
	return p, el
}

func (p *mcpProc) call(t *testing.T, name string, args map[string]any) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t0 := time.Now()
	res, err := p.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	el := time.Since(t0)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: error:\n%s", name, txt)
	}
	return txt, el
}

// rssMiB is the resident set of pid (Linux only; 0 elsewhere).
func rssMiB(pid int) float64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
			return float64(int(kb/1024*10+0.5)) / 10
		}
	}
	return 0
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()/10) / 100 }

func medianDur(v []time.Duration) time.Duration {
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// perfBuffer fills a fake device's log buffer with ≈ size bytes of real
// API 29 logcat (copies of session-raw.log, retimed to end just now).
func perfBuffer(t *testing.T, e fakeadb.Env, serial string, size int) int {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", "api29", "session-raw.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	n := (size + len(text) - 1) / len(text)
	first, last := perf.Span(text)
	span := time.Duration(last-first+60) * time.Second
	end := time.Now().Add(-2 * time.Second)
	shift := end.Sub(time.Unix(last, 0)) - time.Duration(n-1)*span
	f, err := os.OpenFile(filepath.Join(e.Dir, "dev", serial, "buffer"), os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	err = perf.Copies(perf.Retime(text, shift.Truncate(time.Second)), n, time.Minute, func(_ int, c string) error {
		var keep []string
		for _, l := range strings.Split(c, "\n") {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "---------") {
				keep = append(keep, l)
			}
		}
		lines += len(keep)
		_, err := f.WriteString(strings.Join(keep, "\n") + "\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestPerfProcess(t *testing.T) {
	// serial: measures timings and must not share the CPU with other tests
	dir := os.Getenv(perf.EnvOut)
	if dir == "" {
		t.Skip("set " + perf.EnvOut + "=<dir> (scripts/perf.sh)")
	}
	r := perf.New()
	r.Set(perf.CalibKey, perf.Metric{Value: perf.Calibrate(), Unit: "ms", Kind: perf.Info, Desc: "stdlib-only CPU calibration workload (median of 5)"})
	bin := buildBinary(t)
	if fi, err := os.Stat(bin); err == nil {
		r.Set("binary.size_mib", perf.Metric{Value: float64(fi.Size()*10/(1<<20)) / 10, Unit: "MiB", Kind: perf.Mem, Desc: "size of the built CLI binary (copied to <root>/bin by the first client)"})
	}

	// one fake device with an 8 MiB log buffer (≈ 2 MiB per buffer of
	// main, system, crash, events on a real device)
	env, e := lazyEnv(t, "ANDROID_LOG_MCP_LINGER=forever")
	bufLines := perfBuffer(t, e, "FAKEA", 8<<20)

	// cold start: the first MCP process also spawns the service
	var cold []time.Duration
	var p *mcpProc
	for i := 0; i < 3; i++ {
		if i > 0 {
			p.cs.Close()
			cliEnv(t, env, "service", "stop")
			if pid := statePID(t, env); pid > 0 {
				waitGone(t, pid)
			}
		}
		var el time.Duration
		p, el = startReal(t, bin, env)
		cold = append(cold, el)
	}
	svc := statePID(t, env)
	if svc == 0 {
		t.Fatal("no service after the MCP start")
	}
	var warm []time.Duration
	for i := 0; i < 5; i++ {
		q, el := startReal(t, bin, env)
		warm = append(warm, el)
		q.cs.Close()
	}
	c, w := medianDur(cold), medianDur(warm)
	r.Set("startup.tools_list_cold_ms", perf.Metric{Value: msOf(c), Unit: "ms", Kind: perf.Time, Budget: 3000,
		Desc: "MCP process start → initialize + tools/list answered, no service running (spawns it); median of 3"})
	r.Set("startup.tools_list_warm_ms", perf.Metric{Value: msOf(w), Unit: "ms", Kind: perf.Time, Budget: 1000,
		Desc: "MCP process start → initialize + tools/list answered, service already running; median of 5"})
	r.Set("startup.service_spawn_ms", perf.Metric{Value: msOf(c - w), Unit: "ms", Kind: perf.Time,
		Desc: "cold − warm: copying the binary to <root>/bin, starting the service, waiting for its socket"})
	if c >= 0 && len(e.Calls()) != 0 {
		t.Fatalf("adb ran before the first log tool call: %q", e.Calls())
	}

	time.Sleep(time.Second)
	r.Set("mem.service_rss_idle_mib", perf.Metric{Value: rssMiB(svc), Unit: "MiB", Kind: perf.Mem, Budget: 64,
		Desc: "service RSS with a client connected, nothing recorded yet"})
	r.Set("mem.client_rss_mib", perf.Metric{Value: rssMiB(p.cmd.Process.Pid), Unit: "MiB", Kind: perf.Mem, Budget: 64,
		Desc: "RSS of one MCP stdio client process (thin proxy)"})

	// first log-tool call: starts recording and waits for the backfill of
	// the whole buffer
	out, first := p.call(t, "logcat_search", map[string]any{"filter": "message:zz-no-such-text"})
	if strings.Contains(out, "backfill in progress") {
		t.Logf("first call returned before the backfill finished:\n%s", out)
		r.Set("first_call.backfill_incomplete", perf.Metric{Value: 1, Unit: "flag", Kind: perf.Info, Desc: "the first call hit the 10 s catch-up wait"})
	}
	st, _ := p.call(t, "logcat_status", map[string]any{})
	r.Set("first_call.buffer_lines", perf.Metric{Value: float64(bufLines), Unit: "lines", Kind: perf.Info, Desc: "lines in the fake device buffer (8 MiB)"})
	r.Set("first_call.backfill_ms", perf.Metric{Value: msOf(first), Unit: "ms", Kind: perf.Time, Budget: 10000,
		Desc: "first logcat_search: device probe, logcat start and backfill of an 8 MiB buffer into the store"})
	r.Set("first_call.backfill_lines_per_sec", perf.Metric{Value: float64(int(float64(bufLines) / first.Seconds())), Unit: "lines/s", Kind: perf.Rate,
		Desc: "buffer lines / first-call time (the fake adb streams as fast as it can)"})
	if os.Getenv("ALM_PERF_DUMP") != "" {
		t.Logf("first call:\n%s\nstatus:\n%s", out, st)
	}
	time.Sleep(time.Second)
	r.Set("mem.service_rss_recording_mib", perf.Metric{Value: rssMiB(svc), Unit: "MiB", Kind: perf.Mem, Budget: 128,
		Desc: "service RSS recording one device after backfilling 8 MiB"})

	// queries end to end (client → socket → service → text back)
	var warmQ []time.Duration
	for i := 0; i < 5; i++ {
		_, el := p.call(t, "logcat_search", map[string]any{})
		warmQ = append(warmQ, el)
	}
	r.Set("e2e.search_default_ms", perf.Metric{Value: msOf(medianDur(warmQ)), Unit: "ms", Kind: perf.Time, Budget: 1000,
		Desc: "logcat_search {} through MCP client + service, median of 5"})
	for _, q := range []struct {
		name string
		args map[string]any
	}{
		{"logcat_search", map[string]any{"view": "summary"}},
		{"logcat_search", map[string]any{"filter": "message~:\"Displayed .*\"", "limit": 1000}},
		{"logcat_crashes", map[string]any{}},
		{"logcat_app_launches", map[string]any{"from": "24h", "limit": 20}},
		{"logcat_search", map[string]any{"filter": "level:warn", "limit": 1000, "context": 3}},
	} {
		p.call(t, q.name, q.args)
	}
	time.Sleep(time.Second)
	r.Set("mem.service_rss_after_queries_mib", perf.Metric{Value: rssMiB(svc), Unit: "MiB", Kind: perf.Mem, Budget: 192,
		Desc: "service RSS after summary / regex / crashes / launches / context queries over the backfilled data"})

	if err := r.Save(filepath.Join(dir, "process.json")); err != nil {
		t.Fatal(err)
	}
}
