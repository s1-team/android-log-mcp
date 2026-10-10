package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s1crab/android-log-mcp/server/internal/analysis"
	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// fixture builds a store with a launch, a crash and some noise, then a
// Manager without adb that serves it.
func fixture(t *testing.T) *Tools {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(base, "emu-1", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsWriter() && !st.TryPromote() {
		t.Fatal("not writer")
	}
	t0 := time.Now().Add(-10 * time.Minute).Unix()
	ts := func(off float64) string {
		return fmt.Sprintf("%d.%06d", t0+int64(off), int64((off-float64(int64(off)))*1e6+0.5))
	}
	raw := []string{
		"--------- beginning of system",
		ts(0) + "  1000  1880  2100 I ActivityTaskManager: START u0 {act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] flg=0x10200000 cmp=com.foo/.ui.MainActivity} from uid 2000",
		"--------- switch to events",
		ts(0.038) + "  1000  1880  1900 I am_proc_start: [0,23011,10123,com.foo,next-top-activity,{com.foo/com.foo.ui.MainActivity}]",
		ts(0.412) + " 10123 23011 23011 I wm_on_create_called: [123,com.foo.ui.MainActivity,performCreate,186]",
		ts(0.431) + " 10123 23011 23011 I wm_on_start_called: [123,com.foo.ui.MainActivity,handleStartActivity,9]",
		ts(0.440) + " 10123 23011 23011 I wm_on_resume_called: [123,com.foo.ui.MainActivity,RESUME_ACTIVITY,6]",
		"--------- switch to system",
		ts(0.812) + "  1000  1880  1920 I ActivityTaskManager: Displayed com.foo/.ui.MainActivity for user 0: +812ms",
		"--------- switch to main",
	}
	for i := 0; i < 30; i++ {
		raw = append(raw, ts(1+float64(i)*0.5)+fmt.Sprintf(" 10123 23011 23011 D Net: request %d ok", i))
		raw = append(raw, ts(1.1+float64(i)*0.5)+"  1000  1880  1880 W Noise: spam")
	}
	raw = append(raw,
		ts(20)+" 10123 23011 23011 I Repo: loading user",
		"--------- switch to crash",
		ts(21)+" 10123 23011 23011 E AndroidRuntime: FATAL EXCEPTION: main",
		ts(21)+" 10123 23011 23011 E AndroidRuntime: Process: com.foo, PID: 23011",
		ts(21)+" 10123 23011 23011 E AndroidRuntime: java.lang.NullPointerException: user is null",
		ts(21)+" 10123 23011 23011 E AndroidRuntime: \tat com.foo.Repo.load(Repo.kt:42)",
		ts(21)+" 10123 23011 23011 E AndroidRuntime: \tat com.foo.ui.MainActivity.onCreate(MainActivity.kt:10)",
	)
	a := &logcat.Assembler{}
	var recs []*model.Record
	add := func(r *model.Record) {
		if r == nil {
			return
		}
		r.Boot = "b1"
		if r.UID == 10123 {
			r.Package, r.Process = "com.foo", "com.foo"
		}
		if r.PID == 1880 {
			r.Process = "system_server"
		}
		recs = append(recs, r)
	}
	for _, ln := range raw {
		add(a.Push(ln))
	}
	add(a.Flush())
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ, m.Model = "emu-1", "UTC", "Pixel Test"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
	})
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	// crash detection as the recorder would do it
	det := analysis.NewDetector()
	n := 0
	det.Emit = func(c *analysis.Crash) {
		if c.ID == "" {
			n++
			c.ID = fmt.Sprintf("cr%d", n)
		}
		st.AppendAux("crashes", c)
	}
	for _, r := range recs {
		det.Feed(r)
	}
	det.Tick(time.Now().Add(time.Hour).UnixNano())
	st.Close()

	m := device.NewManager(base, store.Config{})
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	return &Tools{M: m}
}

func call(t *testing.T, tl *Tools, name, args string) (string, bool) {
	t.Helper()
	return tl.Call(context.Background(), name, json.RawMessage(args))
}

func must(t *testing.T, out string, isErr bool, want ...string) {
	t.Helper()
	if isErr {
		t.Fatalf("unexpected error:\n%s", out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestSearchLines(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"filter":"tag:Net","limit":5}`)
	must(t, out, e, "filter parsed as:", "matched 30 lines; showing newest 5", "Net: request 29 ok", "before=\"c1.")
	if strings.Contains(out, "request 24 ok") {
		t.Error("limit not applied")
	}
	// cursor paging
	i := strings.Index(out, `before="`)
	cur := out[i+8:]
	cur = cur[:strings.IndexByte(cur, '"')]
	out2, e := call(t, tl, "logcat_search", `{"filter":"tag:Net","limit":5,"before":"`+cur+`"}`)
	must(t, out2, e, "request 24 ok", "request 20 ok")
	if strings.Contains(out2, "request 25 ok") {
		t.Errorf("page overlap:\n%s", out2)
	}
}

func TestSearchPackageAndCollapse(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"filter":"tag:Noise","limit":50}`)
	must(t, out, e, "identical")
	out, e = call(t, tl, "logcat_search", `{"package":"com.foo","min_level":"error"}`)
	must(t, out, e, "FATAL EXCEPTION")
	if strings.Contains(out, "Noise") {
		t.Error("package filter leaked")
	}
}

func TestSearchSummary(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_search", `{"view":"summary"}`)
	must(t, out, e, "top tags:", "Noise", "crashes in range: 1")
}

func TestSearchErrors(t *testing.T) {
	tl := fixture(t)
	for args, want := range map[string]string{
		`{"filter":"tag:Net foo:bar"}`: `unknown key "foo"`,
		`{"filter":"level:loud"}`:      "level",
		`{"filter":"tag:a && tag:b"}`:  "&&",
		`{"filtr":"x"}`:                "unknown parameter",
		`{"limit":"5"}`:                "wrong type",
		`{"device":"nope"}`:            "nope",
	} {
		out, isErr := call(t, tl, "logcat_search", args)
		if !isErr || !strings.Contains(out, want) {
			t.Errorf("%s: isErr=%v, want %q in:\n%s", args, isErr, want, out)
		}
	}
}

func TestCrashes(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_crashes", `{}`)
	must(t, out, e, "cr1", "NullPointerException", "Repo.load", "before crash", "Repo: loading user")
	out, isErr := call(t, tl, "logcat_crashes", `{"id":"cr9"}`)
	if !isErr || !strings.Contains(out, "cr1") {
		t.Errorf("bad id: %s", out)
	}
	out, e = call(t, tl, "logcat_crashes", `{"package":"com.bar"}`)
	must(t, out, e, "no crashes")
	out, e = call(t, tl, "logcat_crashes", `{"kinds":["anr"]}`)
	must(t, out, e, "no crashes")
	if out, isErr := call(t, tl, "logcat_crashes", `{"kinds":["oom"]}`); !isErr {
		t.Errorf("kinds: %s", out)
	}
}

func TestLaunches(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_app_launches", `{"package":"com.foo"}`)
	must(t, out, e, "1 launches", "COLD", "com.foo/.ui.MainActivity", "+440 ms", "TTID (time to initial display", "812")
}

func TestStatus(t *testing.T) {
	tl := fixture(t)
	out, e := call(t, tl, "logcat_status", `{}`)
	must(t, out, e, "emu-1", "cursor")
}

func TestTrimStack(t *testing.T) {
	var b strings.Builder
	b.WriteString("java.lang.StackOverflowError\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "\tat com.foo.A.f%d(A.kt:1)\n", i)
	}
	s := trimStack(b.String())
	if len(s) > 5000 || !strings.Contains(s, "frames omitted") || !strings.Contains(s, "f1999") || !strings.Contains(s, "f39(") || strings.Contains(s, "f40(") {
		t.Errorf("trim: %d\n%s", len(s), s)
	}
}

// End-to-end over the MCP protocol with in-memory transports.
func TestMCPInMemory(t *testing.T) {
	tl := fixture(t)
	ctx := context.Background()
	srv := NewServer(tl)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range lt.Tools {
		names = append(names, x.Name)
		if x.Annotations == nil || !x.Annotations.ReadOnlyHint {
			t.Errorf("%s not read-only", x.Name)
		}
	}
	if strings.Join(names, ",") != "logcat_app_launches,logcat_crashes,logcat_search,logcat_status" {
		t.Errorf("tools: %v", names)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "logcat_search", Arguments: map[string]any{"filter": "tag:Net", "limit": 2}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "Net: request 29 ok") {
		t.Errorf("%s", txt)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "logcat_search", Arguments: map[string]any{"filter": "(tag:Net"}})
	if err != nil || !res.IsError {
		t.Errorf("expected tool error, got %v %+v", err, res)
	}
}
