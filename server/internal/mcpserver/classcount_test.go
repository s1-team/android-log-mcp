package mcpserver

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/filter"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Property: for random countable filters × windows, CountMatch == full scan
// (issue #37).
func TestClassCountMatchesScanProperty(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "dev")
	cfg := store.Config{BlockBytes: 4 << 10, SegmentBytes: 64 << 10, MemRecords: 80}
	s, err := store.OpenDir(dir, "dev", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rng := rand.New(rand.NewSource(37))
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	tags := []string{"ActivityManager", "chatty", "OkHttp", "MyApp"}
	pkgs := []string{"com.example.a", "com.example.b", "android"}
	levels := []model.Level{model.Debug, model.Info, model.Warn, model.Error}
	const N = 800
	for i := 0; i < N; i++ {
		if err := s.Append([]*model.Record{{
			Boot: "boot", TimeNs: now + int64(i)*int64(time.Millisecond),
			UID: 10000 + i%5, PID: 100 + i%11, TID: 1,
			Level: levels[i%len(levels)], Tag: tags[i%len(tags)],
			Msg: fmt.Sprintf("line %d payload", i), Process: "proc",
			Package: pkgs[i%len(pkgs)],
		}}); err != nil {
			t.Fatal(err)
		}
	}
	s.EnsureIndex()
	queries := []string{
		`min_level_via_level`, // handled below as Level node
		`tag=:ActivityManager`,
		`tag:chatty`,
		`package=:com.example.a`,
		`level:warn`,
		`level:error tag=:OkHttp`,
		`tag=:ActivityManager | tag=:OkHttp`,
		`-tag:chatty level:info`,
	}
	ectx := &filter.Ctx{Now: time.Unix(0, now+int64(N)*int64(time.Millisecond)), Loc: time.UTC}
	for _, q := range queries {
		var pred filter.Node
		switch q {
		case `min_level_via_level`:
			pred = &filter.Level{Min: model.Error}
		default:
			p, err := filter.Parse(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			pred = p.Root
		}
		if !filter.Countable(pred) {
			t.Fatalf("%s not countable", q)
		}
		match := func(r *model.Record) bool { return pred.Match(r, ectx) }
		ids := s.MatchingClassIDs(match)
		// a few random windows
		for trial := 0; trial < 8; trial++ {
			a := uint64(rng.Intn(N / 4))
			b := a + uint64(rng.Intn(N/2)+10)
			if b > N+1 {
				b = N + 1
			}
			from := now + int64(rng.Intn(N/3))*int64(time.Millisecond)
			to := from + int64(100+rng.Intn(400))*int64(time.Millisecond)
			if trial%3 == 0 {
				from, to = 0, 0 // unbounded
			}
			got := s.CountMatch(a, b, from, to, ids, match)
			var want uint64
			s.ScanTime(a, b, true, from, to, func(r *model.Record) bool {
				if match(r) {
					want++
				}
				return true
			})
			if got != want {
				t.Fatalf("%s trial=%d after=%d before=%d from=%d to=%d: CountMatch=%d scan=%d",
					q, trial, a, b, from, to, got, want)
			}
		}
	}
}

func TestFilterCountable(t *testing.T) {
	t.Parallel()
	yes := []string{`tag:a`, `tag=:A`, `package:foo`, `process:bar`, `level:error`, `tag:a level:warn`, `tag:a | tag:b`}
	no := []string{`message:x`, `line:x`, `pid:1`, `is:crash`, `tag:a message:x`, `age:5m | tag:a`}
	for _, q := range yes {
		p, err := filter.Parse(q)
		if err != nil {
			t.Fatal(q, err)
		}
		if !filter.Countable(p.Root) {
			t.Fatalf("want countable: %s", q)
		}
	}
	for _, q := range no {
		p, err := filter.Parse(q)
		if err != nil {
			t.Fatal(q, err)
		}
		if filter.Countable(p.Root) {
			t.Fatalf("want not countable: %s", q)
		}
	}
	if !filter.Countable(filter.Empty{}) {
		t.Fatal("Empty")
	}
	if !filter.Countable(&filter.Pred{ClassOnly: true}) {
		t.Fatal("ClassOnly Pred")
	}
	if filter.Countable(&filter.Pred{}) {
		t.Fatal("plain Pred")
	}
}
