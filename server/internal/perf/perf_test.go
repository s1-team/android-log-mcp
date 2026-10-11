package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/filter"
	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/mcpserver"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

// In-process measurements (issue #13). TestPerfInProcess runs only with
// ALM_PERF_OUT=<dir> (scripts/perf.sh); the Benchmark* functions are the
// same bodies for ad-hoc `go test -bench . ./internal/perf`.

func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }

const (
	fxSerial = "emulator-5556"
	fxPkg    = "com.example.logtest"
	fxBoot   = "07a6d2d3"
	fxZone   = "Asia/Kuching"
	fxOffset = "+0800"
	// default number of back-to-back copies of session-raw.log in the
	// synthetic history (ALM_PERF_COPIES overrides)
	defaultCopies = 80
)

const fxPackages = `package:com.example.logtest uid:10146
package:com.google.android.apps.nexuslauncher uid:10108
package:com.android.systemui uid:10110
package:com.android.nfc uid:1027
package:android uid:1000
package:com.android.settings uid:1000
package:com.android.providers.media uid:10056
`

func fixture(tb testing.TB, name string) string {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(FixtureDir, name))
	if err != nil {
		tb.Fatal(err)
	}
	return string(b)
}

func fxLoc() *time.Location {
	loc, _ := device.ResolveZone(fxZone, fxOffset, time.Unix(1791572000, 0))
	return loc
}

// newStore opens a writer store for the fixture device with one boot
// session, the way the recorder prepares it.
func newStore(tb testing.TB, base string, cfg store.Config) (*store.Store, *device.Pipeline) {
	tb.Helper()
	st, err := store.Open(base, fxSerial, cfg)
	if err != nil || !st.IsWriter() {
		tb.Fatalf("open store: %v", err)
	}
	next := st.LastSeq() + 1
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.Model, m.Release, m.SDK = fxSerial, "Android SDK built for x86_64", "10", "29"
		m.TZ, m.TZOffset = fxZone, fxOffset
		m.Sessions = append(m.Sessions, store.Session{BootID: fxBoot + "-0000", Boot: fxBoot, FirstSeq: next})
		m.LastBootID = fxBoot + "-0000"
	})
	device.WritePackages(st, procs.ParsePackagesU(fxPackages))
	return st, device.NewPipeline(st, fxBoot, fxLoc())
}

// setNow makes the device clock (Device.Now) read `at`.
func setNow(st *store.Store, at time.Time) {
	now := time.Now()
	st.UpdateMeta(func(m *store.Meta) {
		m.ClockOffsetNs = at.Sub(now).Nanoseconds()
		m.ClockHostNs = now.UnixNano()
	})
	st.Flush()
}

// openTools opens a dataset read-only through the production manager (as
// --offline does) and returns the tools.
func openTools(tb testing.TB, base string, cfg store.Config) (*mcpserver.Tools, *device.Manager) {
	tb.Helper()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	tb.Cleanup(m.Close)
	return &mcpserver.Tools{M: m}, m
}

// fixtureDataset is session-raw.log (15 min of a real API 29 emulator:
// cold/hot starts, Java/native crashes, ANR, :remote process) plus the
// DropBox captures; device clock = 1 min after its last line.
func fixtureDataset(tb testing.TB) (*mcpserver.Tools, *device.Manager, string) {
	tb.Helper()
	base := tb.TempDir()
	st, p := newStore(tb, base, store.Config{})
	text := fixture(tb, "session-raw.log")
	if err := p.ReplayText(text); err != nil {
		tb.Fatal(err)
	}
	for _, f := range []string{"dropbox-data_app_crash.txt", "dropbox-data_app_native_crash.txt", "dropbox-SYSTEM_TOMBSTONE.txt", "dropbox-data_app_anr.txt"} {
		p.ApplyDropbox(fixture(tb, f))
	}
	_, last := Span(text)
	setNow(st, time.Unix(last+60, 0))
	st.Close()
	tools, m := openTools(tb, base, store.Config{})
	return tools, m, base
}

type scenario struct{ name, tool, args string }

// The standard agent scenarios on the fixture (absolute times, so the
// output is the same on every run and machine).
var outputScenarios = []scenario{
	{"status", "logcat_status", `{}`},
	{"status_package", "logcat_status", `{"package":"com.example.logtest"}`},
	{"search_default", "logcat_search", `{}`},
	{"search_package", "logcat_search", `{"package":"com.example.logtest"}`},
	{"search_errors", "logcat_search", `{"min_level":"error"}`},
	{"search_tag_limit20", "logcat_search", `{"filter":"tag=:ActivityManager","limit":20}`},
	{"search_crash_context", "logcat_search", `{"filter":"is:crash","limit":5,"context":3}`},
	{"search_summary", "logcat_search", `{"view":"summary"}`},
	{"search_no_match", "logcat_search", `{"filter":"message:zz-no-such-text"}`},
	{"crashes_default", "logcat_crashes", `{}`},
	{"crashes_package", "logcat_crashes", `{"package":"com.example.logtest"}`},
	{"crashes_detail_java", "logcat_crashes", `{"id":"cr1"}`},
	{"launches_package", "logcat_app_launches", `{"package":"com.example.logtest","from":"2026-10-10 02:50:00"}`},
	{"launches_default", "logcat_app_launches", `{}`},
}

// normalize removes what depends on the machine or the moment (temp
// paths, host-clock durations) so the size is comparable.
func normalize(out, base string) string {
	return strings.ReplaceAll(out, base, "<root>")
}

func callOK(tb testing.TB, t *mcpserver.Tools, tool, args string) string {
	tb.Helper()
	out, isErr := t.Call(context.Background(), tool, json.RawMessage(args))
	if isErr {
		tb.Fatalf("%s %s: error:\n%s", tool, args, out)
	}
	return out
}

func outputMetrics(t *testing.T, r *Results) {
	tools, _, base := fixtureDataset(t)
	for _, s := range outputScenarios {
		out := normalize(callOK(t, tools, s.tool, s.args), base)
		again := normalize(callOK(t, tools, s.tool, s.args), base)
		if out != again {
			t.Errorf("%s: output not deterministic", s.name)
		}
		if os.Getenv("ALM_PERF_DUMP") != "" {
			t.Logf("=== %s %s %s (%d bytes)\n%s", s.name, s.tool, s.args, len(out), out)
		}
		r.Set("out."+s.name+".bytes", Metric{Value: float64(len(out)), Unit: "bytes", Kind: Exact,
			Desc: fmt.Sprintf("%s %s on the API 29 fixture (≈ %d tokens)", s.tool, s.args, len(out)/4)})
	}
	// the fixed per-session cost: what an MCP client loads into the
	// model's context before any call (initialize instructions + tools/list)
	init, list := handshakeBytes(t, tools)
	r.Set("out.initialize.bytes", Metric{Value: float64(init), Unit: "bytes", Kind: Exact, Desc: "initialize result (server info + instructions)"})
	r.Set("out.tools_list.bytes", Metric{Value: float64(list), Unit: "bytes", Kind: Exact, Desc: "tools/list result (4 tool definitions)"})
}

// handshakeBytes returns the JSON sizes of the initialize and tools/list
// results as a client receives them.
func handshakeBytes(t *testing.T, tools *mcpserver.Tools) (int, int) {
	ctx := context.Background()
	srv := mcpserver.NewServer(tools)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "perf", Version: "1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ir, _ := json.Marshal(cs.InitializeResult())
	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	lb, _ := json.Marshal(lr)
	return len(ir), len(lb)
}

// ---- allocations (deterministic-ish) ----

func fixtureLines(tb testing.TB) []string {
	var out []string
	for _, l := range strings.Split(fixture(tb, "session-raw.log"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func fixtureRecords(tb testing.TB) []*model.Record {
	asm := &logcat.Assembler{Loc: fxLoc(), Year: 2026}
	var recs []*model.Record
	for _, l := range fixtureLines(tb) {
		if r := asm.Push(l); r != nil {
			recs = append(recs, r)
		}
	}
	if r := asm.Flush(); r != nil {
		recs = append(recs, r)
	}
	return recs
}

// BenchmarkParseLine: one physical logcat line → fields. op = 1 line.
func BenchmarkParseLine(b *testing.B) {
	lines := fixtureLines(b)
	loc := fxLoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logcat.ParseLine(lines[i%len(lines)], loc, 2026)
	}
}

// BenchmarkIngest: the whole ingest path (parse, assemble multi-line
// messages, process tracking, crash detection, JSONL append) for one copy
// of session-raw.log into a fresh store (so every op does the same work).
// op = 1 copy (7548 lines); allocMetrics divides by the line count.
func BenchmarkIngest(b *testing.B) {
	text := fixture(b, "session-raw.log")
	root := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		st, p := newStore(b, filepath.Join(root, strconv.Itoa(i)), store.Config{})
		b.StartTimer()
		if err := p.ReplayText(text); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		st.Close()
		os.RemoveAll(filepath.Join(root, strconv.Itoa(i)))
		b.StartTimer()
	}
}

var filterBenches = []struct{ name, expr string }{
	{"tag_level", `tag:ActivityManager level:warn`},
	{"regex", `message~:"Displayed .*MainActivity"`},
	{"negation", `-tag:chatty -message:binder level:info`},
	{"bare_word", `logtest`},
}

// benchFilter: evaluating a parsed filter on records (canonical line
// cached, as after the first query). op = 1 record.
func benchFilter(expr string) func(b *testing.B) {
	return func(b *testing.B) {
		recs := fixtureRecords(b)
		p, err := filter.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		ctx := &filter.Ctx{Now: time.Unix(1791573000, 0), Loc: fxLoc()}
		for _, r := range recs {
			p.Root.Match(r, ctx)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p.Root.Match(recs[i%len(recs)], ctx)
		}
	}
}

func BenchmarkFilter(b *testing.B) {
	for _, f := range filterBenches {
		b.Run(f.name, benchFilter(f.expr))
	}
}

// BenchmarkFilterParse: parsing and validating a typical filter. op = 1 parse.
func BenchmarkFilterParse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := filter.Parse(`tag=:OkHttp tag=:MyRepo level:debug -message~:"heart(beat)?" | is:crash`); err != nil {
			b.Fatal(err)
		}
	}
}

var callBenches = []scenario{
	{"search_default", "logcat_search", `{}`},
	{"search_package_errors", "logcat_search", `{"package":"com.example.logtest","min_level":"warn"}`},
	{"search_regex", "logcat_search", `{"filter":"message~:\"Displayed .*MainActivity\""}`},
	{"search_summary", "logcat_search", `{"view":"summary"}`},
	{"crashes_default", "logcat_crashes", `{}`},
	{"launches_package", "logcat_app_launches", `{"package":"com.example.logtest","from":"2026-10-10 02:50:00"}`},
	{"status", "logcat_status", `{}`},
}

// benchCall: one tool call end to end (args decoding, filter, scan of the
// fixture's ~6k records, rendering). op = 1 call.
func benchCall(tools *mcpserver.Tools, s scenario) func(b *testing.B) {
	return func(b *testing.B) {
		callOK(b, tools, s.tool, s.args)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tools.Call(context.Background(), s.tool, json.RawMessage(s.args))
		}
	}
}

func BenchmarkCall(b *testing.B) {
	tools, _, _ := fixtureDataset(b)
	for _, s := range callBenches {
		b.Run(s.name, benchCall(tools, s))
	}
}

func allocMetrics(t *testing.T, r *Results) {
	set := func(name, desc string, res testing.BenchmarkResult, per float64) {
		if res.N == 0 {
			t.Fatalf("benchmark %s did not run", name)
		}
		r.Set("allocs."+name+".allocs_per_op", Metric{Value: round(float64(res.AllocsPerOp())/per, 2), Unit: "allocs/op", Kind: Allocs, Desc: desc})
		r.Set("allocs."+name+".bytes_per_op", Metric{Value: round(float64(res.AllocedBytesPerOp())/per, 0), Unit: "B/op", Kind: Allocs, Desc: desc})
		r.Set("time."+name+".ns_per_op", Metric{Value: round(float64(res.NsPerOp())/per, 1), Unit: "ns/op", Kind: Info, Desc: desc})
	}
	set("parse_line", "logcat.ParseLine, per physical line", testing.Benchmark(BenchmarkParseLine), 1)
	lines := float64(len(fixtureLines(t)))
	set("ingest_line", "parse + assemble + pipeline + store append, per physical line", testing.Benchmark(BenchmarkIngest), lines)
	set("filter_parse", "filter.Parse of a typical 6-term filter", testing.Benchmark(BenchmarkFilterParse), 1)
	for _, f := range filterBenches {
		set("filter_match."+f.name, "filter evaluation per record: "+f.expr, testing.Benchmark(benchFilter(f.expr)), 1)
	}
	tools, _, _ := fixtureDataset(t)
	for _, s := range callBenches {
		set("call."+s.name, fmt.Sprintf("%s %s on the fixture, per call", s.tool, s.args), testing.Benchmark(benchCall(tools, s)), 1)
	}
}

func round(v float64, digits int) float64 {
	p := 1.0
	for i := 0; i < digits; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

// ---- scale: synthetic history ----

// largeDataset ingests `copies` back-to-back copies of session-raw.log
// (≈ 7.5k lines / 16 min each) into a fresh store and reports ingest
// throughput and disk usage.
func largeDataset(t *testing.T, r *Results, copies int) (base string, first, last time.Time) {
	base = t.TempDir()
	text := fixture(t, "session-raw.log")
	perCopy := CountLines(text)
	st, p := newStore(t, base, store.Config{})
	var ingest time.Duration
	var lastText string
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	err := Copies(text, copies, time.Minute, func(k int, c string) error {
		t0 := time.Now()
		err := p.ReplayText(c)
		ingest += time.Since(t0)
		lastText = c
		if k == 0 {
			f, _ := Span(c)
			first = time.Unix(f, 0)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	_, l := Span(lastText)
	last = time.Unix(l, 0)
	lines := perCopy * copies
	// Retime's output is part of the delta; subtract it (one string per copy)
	retimed := uint64(len(text)) * uint64(copies)
	r.Set("allocs.ingest_at_scale.bytes_per_line", Metric{Value: round(float64(m1.TotalAlloc-m0.TotalAlloc-retimed)/float64(lines), 0), Unit: "B/op", Kind: Allocs,
		Desc: "bytes allocated per line over the whole synthetic history (grows if ingest work grows with history size)"})
	r.Set("allocs.ingest_at_scale.allocs_per_line", Metric{Value: round(float64(m1.Mallocs-m0.Mallocs)/float64(lines), 1), Unit: "allocs/op", Kind: Allocs,
		Desc: "allocations per line over the whole synthetic history"})
	recs := int(st.LastSeq())
	setNow(st, last.Add(time.Minute))
	st.Close()
	var segBytes, auxBytes int64
	dir := st.Dir
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "seg-") {
			segBytes += fi.Size()
		} else {
			auxBytes += fi.Size()
		}
	}
	r.Set("scale.lines", Metric{Value: float64(lines), Unit: "lines", Kind: Info, Desc: fmt.Sprintf("synthetic history: %d copies of session-raw.log", copies)})
	r.Set("scale.records", Metric{Value: float64(recs), Unit: "records", Kind: Info, Desc: "stored records (multi-line messages merged)"})
	r.Set("ingest.lines_per_sec", Metric{Value: round(float64(lines)/ingest.Seconds(), 0), Unit: "lines/s", Kind: Rate, Budget: 20_000,
		Desc: "parse + assemble + pipeline + JSONL append, single goroutine"})
	r.Set("disk.bytes_per_line", Metric{Value: round(float64(segBytes)/float64(lines), 1), Unit: "bytes", Kind: Exact, Desc: "segment bytes per input logcat line"})
	r.Set("disk.bytes_per_record", Metric{Value: round(float64(segBytes)/float64(recs), 1), Unit: "bytes", Kind: Exact, Desc: "segment bytes per stored record"})
	r.Set("disk.aux_bytes", Metric{Value: float64(auxBytes), Unit: "bytes", Kind: Info, Desc: "procs/crashes/meta files of the synthetic history"})
	r.Set("disk.raw_bytes_per_line", Metric{Value: round(float64(len(text))/float64(perCopy), 1), Unit: "bytes", Kind: Info, Desc: "the logcat text itself, for comparison"})
	return base, first, last
}

type queryCase struct {
	name, tool, args string
	cold             bool // also measure after dropping the history cache
}

func queryCases(first, last time.Time, loc *time.Location) []queryCase {
	old := first.Add(5 * time.Minute).In(loc)
	f := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	return []queryCase{
		{"status", "logcat_status", `{}`, false},
		{"status_package", "logcat_status", `{"package":"com.example.logtest"}`, false},
		{"search_default", "logcat_search", `{}`, false},
		{"search_package", "logcat_search", `{"package":"com.example.logtest"}`, false},
		{"search_tag", "logcat_search", `{"filter":"tag=:ActivityManager"}`, false},
		{"search_level_error", "logcat_search", `{"min_level":"error"}`, false},
		{"search_regex", "logcat_search", `{"filter":"message~:\"Displayed .*MainActivity\""}`, false},
		{"search_negation", "logcat_search", `{"filter":"-tag:chatty -message:binder level:info"}`, false},
		{"search_age", "logcat_search", `{"filter":"age:5m level:warn"}`, false},
		{"search_old_range", "logcat_search", fmt.Sprintf(`{"from":%q,"to":%q}`, f(old), f(old.Add(5*time.Minute))), true},
		{"search_full_scan_no_match", "logcat_search", `{"filter":"message:zz-no-such-text"}`, true},
		{"search_summary_1h", "logcat_search", `{"view":"summary","from":"1h"}`, false},
		{"crashes_default", "logcat_crashes", `{}`, false},
		{"crashes_package", "logcat_crashes", `{"package":"com.example.logtest"}`, false},
		{"crashes_detail_oldest", "logcat_crashes", `{"id":"cr1"}`, true},
		{"launches_package", "logcat_app_launches", `{"package":"com.example.logtest"}`, false},
		{"launches_package_24h", "logcat_app_launches", `{"package":"com.example.logtest","from":"24h","limit":20}`, true},
	}
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func ms(d time.Duration) float64 { return round(float64(d.Microseconds())/1000, 2) }

func scaleMetrics(t *testing.T, r *Results) {
	copies := defaultCopies
	if v, err := strconv.Atoi(os.Getenv("ALM_PERF_COPIES")); err == nil && v > 0 {
		copies = v
	}
	base, first, last := largeDataset(t, r, copies)
	runtime.GC()
	tools, m := openTools(t, base, store.Config{})
	d := m.Get(fxSerial)
	if d == nil || d.Store == nil {
		t.Fatal("no device in the synthetic dataset")
	}
	scanMetrics(t, r, d.Store)
	for _, q := range queryCases(first, last, fxLoc()) {
		if q.cold {
			d.Store.ReleaseCaches()
			t0 := time.Now()
			out := callOK(t, tools, q.tool, q.args)
			el := time.Since(t0)
			r.Set("query."+q.name+".cold_ms", Metric{Value: ms(el), Unit: "ms", Kind: Time, Budget: 4000,
				Desc: fmt.Sprintf("%s %s, history cache dropped first", q.tool, q.args)})
			noteIncomplete(t, r, q.name, out)
		}
		var runs []float64
		var out string
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			out = callOK(t, tools, q.tool, q.args)
			runs = append(runs, ms(time.Since(t0)))
		}
		r.Set("query."+q.name+".ms", Metric{Value: median(runs), Unit: "ms", Kind: Time, Budget: 3500,
			Desc: fmt.Sprintf("%s %s, median of 5 (warm)", q.tool, q.args)})
		noteIncomplete(t, r, q.name, out)
		noteCountExact(r, q.name, out)
		r.Set("query."+q.name+".bytes", Metric{Value: float64(len(out)), Unit: "bytes", Kind: Info,
			Desc: "output size at scale (≈ tokens × 4)"})
		if os.Getenv("ALM_PERF_DUMP") != "" {
			t.Logf("=== %s %s %s (%d bytes)\n%s", q.name, q.tool, q.args, len(out), out)
		}
	}
	runtime.GC()
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	cache := d.Store.HistoryCacheBytes()
	r.Set("mem.history_cache_after_queries_mib", Metric{Value: round(float64(cache)/(1<<20), 1), Unit: "MiB", Kind: Mem,
		Budget: float64(store.DefaultHistoryCacheBytes) / (1 << 20), Desc: "decoded history blocks kept (limit 32 MiB)"})
	r.Set("mem.heap_after_queries_mib", Metric{Value: round(float64(ms1.HeapAlloc)/(1<<20), 1), Unit: "MiB", Kind: Mem,
		Desc: "live heap after all queries (recent window + history cache + indexes), after GC"})
	freed := d.Store.ReleaseCaches()
	runtime.GC()
	var ms2 runtime.MemStats
	runtime.ReadMemStats(&ms2)
	r.Set("mem.heap_after_release_mib", Metric{Value: round(float64(ms2.HeapAlloc)/(1<<20), 1), Unit: "MiB", Kind: Mem,
		Desc: fmt.Sprintf("live heap after the idle release (%.1f MiB freed)", float64(freed)/(1<<20))})
}

// scanMetrics measures the raw store scan over the whole history (no time
// budget, unlike the tools), newest first as the tools read: cold (history
// cache dropped: read + decode JSONL) and warm, and with a filter.
func scanMetrics(t *testing.T, r *Results, st *store.Store) {
	p, err := filter.Parse(`-tag:chatty -message:binder level:info`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &filter.Ctx{Now: time.Now(), Loc: fxLoc()}
	scan := func(withFilter bool) (int, time.Duration) {
		n := 0
		t0 := time.Now()
		st.Scan(0, st.LastSeq()+1, true, func(rec *model.Record) bool {
			n++
			if withFilter {
				p.Root.Match(rec, ctx)
			}
			return true
		})
		return n, time.Since(t0)
	}
	st.ReleaseCaches()
	n, cold := scan(false)
	_, warm := scan(false)
	st.ReleaseCaches()
	_, coldF := scan(true)
	rate := func(d time.Duration) float64 { return round(float64(n)/d.Seconds(), 0) }
	r.Set("scan.records_per_sec_cold", Metric{Value: rate(cold), Unit: "records/s", Kind: Rate,
		Desc: fmt.Sprintf("Store.Scan of all %d records newest first, history cache dropped (read + decode)", n)})
	r.Set("scan.records_per_sec_warm", Metric{Value: rate(warm), Unit: "records/s", Kind: Rate,
		Desc: "the same scan right after (blocks still cached up to the 32 MiB limit)"})
	r.Set("scan.filtered_records_per_sec_cold", Metric{Value: rate(coldF), Unit: "records/s", Kind: Rate,
		Desc: "cold scan plus filter evaluation (-tag:chatty -message:binder level:info)"})
}

var countExactRe = regexp.MustCompile(`(?m)^count: total=\d+ count_exact=(true|false)`)

// noteCountExact records whether a search's match count is exact (1) or a
// lower bound (0) (issue #20). A search without a filter must stay exact
// (Store.Count): budget 1, any machine.
func noteCountExact(r *Results, name, out string) {
	m := countExactRe.FindStringSubmatch(out)
	if m == nil {
		return // not a lines search
	}
	v := 0.0
	if m[1] == "true" {
		v = 1
	}
	met := Metric{Value: v, Unit: "flag", Kind: Info, Desc: "1: the match count is exact; 0: a lower bound (count stopped 100 ms after the page was full)"}
	if name == "search_default" {
		met.Kind, met.Budget = Rate, 1
		met.Desc += "; must be exact without a filter"
	}
	r.Set("query."+name+".count_exact", met)
}

// noteIncomplete records whether a query hit the scan time budget (its
// answer then covers only part of the range).
func noteIncomplete(t *testing.T, r *Results, name, out string) {
	if strings.Contains(out, "scan stopped after") {
		t.Logf("note: %s hit the scan budget", name)
		r.Set("query."+name+".incomplete", Metric{Value: 1, Unit: "flag", Kind: Info, Desc: "the scan time budget stopped the query"})
	}
}

// TestPerfInProcess writes <ALM_PERF_OUT>/inprocess.json.
func TestPerfInProcess(t *testing.T) {
	dir := os.Getenv(EnvOut)
	if dir == "" {
		t.Skip("set " + EnvOut + "=<dir> (scripts/perf.sh)")
	}
	r := New()
	r.Set(CalibKey, Metric{Value: Calibrate(), Unit: "ms", Kind: Info, Desc: "stdlib-only CPU calibration workload (median of 5)"})
	outputMetrics(t, r)
	allocMetrics(t, r)
	scaleMetrics(t, r)
	if err := r.Save(filepath.Join(dir, "inprocess.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPerfDumpOutputs(t *testing.T) {
	if os.Getenv("ALM_PERF_DUMP") == "" {
		t.Skip("set ALM_PERF_DUMP=1 to print the scenario outputs")
	}
	outputMetrics(t, New())
}
