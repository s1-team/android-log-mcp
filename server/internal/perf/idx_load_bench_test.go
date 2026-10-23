package perf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/procs"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

var errStopMeasure = errors.New("stop measure ingest")

// Manual: MEASURE_512=1 go test -run TestIdxLoadMS512 -count=1 -timeout 45m ./internal/perf/
func TestIdxLoadMS512(t *testing.T) {
	if os.Getenv("MEASURE_512") != "1" {
		t.Skip("set MEASURE_512=1 to run")
	}
	root := os.Getenv("MEASURE_ROOT")
	if root == "" {
		root = "/tmp/alm-g1-512-store"
	}
	raw := fixture(t, "session-raw.log")
	text := []byte(raw)
	os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		fxSerial = "emulator-5556"
		fxBoot   = "07a6d2d3"
		fxZone   = "Asia/Kuching"
		fxOffset = "+0800"
	)
	cfg := store.Config{
		MaxBytes:    512 << 20,
		MaxAge:      365 * 24 * time.Hour,
		IdleRelease: -1,
	}
	st, err := store.Open(root, fxSerial, cfg)
	if err != nil || !st.IsWriter() {
		t.Fatalf("open: %v writer=%v", err, st != nil && st.IsWriter())
	}
	loc, _ := device.ResolveZone(fxZone, fxOffset, time.Unix(1791572000, 0))
	next := st.LastSeq() + 1
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.Model, m.Release, m.SDK = fxSerial, "Android SDK built for x86_64", "10", "29"
		m.TZ, m.TZOffset = fxZone, fxOffset
		m.Sessions = append(m.Sessions, store.Session{BootID: fxBoot + "-0000", Boot: fxBoot, FirstSeq: next})
		m.LastBootID = fxBoot + "-0000"
	})
	device.WritePackages(st, procs.ParsePackagesU(`package:com.example.logtest uid:10146
package:android uid:1000
`))
	p := device.NewPipeline(st, fxBoot, loc)
	target := int64(512) << 20
	var copies int
	err = Copies(string(text), 400, time.Minute, func(k int, c string) error {
		if err := p.ReplayText(c); err != nil {
			return err
		}
		copies = k + 1
		if k%10 == 9 || k == 0 {
			var segB int64
			for _, sg := range store.ListSegments(st.Dir) {
				segB += sg.Size
			}
			t.Logf("ingest copy=%d mib=%.1f", copies, float64(segB)/(1<<20))
			if segB >= target-int64(16<<20) && copies >= 200 {
				return errStopMeasure
			}
		}
		return nil
	})
	if err != nil && err != errStopMeasure {
		t.Fatal(err)
	}
	st.Flush()
	var bytes int64
	segs := store.ListSegments(st.Dir)
	for _, s := range segs {
		bytes += s.Size
	}
	t.Logf("ingest copies=%d segs=%d retained_mib=%.2f", copies, len(segs), float64(bytes)/(1<<20))
	if bytes < 400<<20 {
		t.Fatalf("fixture too small: %d", bytes)
	}

	st.RemoveIndexSidecars()
	st.DropIndexes()
	t0 := time.Now()
	st.EnsureIndex()
	build := time.Since(t0)
	t.Logf("index.build_ms=%.2f", float64(build.Microseconds())/1000)

	nIdx := 0
	ents, _ := os.ReadDir(st.Dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".idx" {
			nIdx++
		}
	}
	t.Logf("sidecars=%d closed=%d", nIdx, max(0, len(segs)-1))

	var loads []float64
	for i := 0; i < 5; i++ {
		st.DropIndexes()
		t0 = time.Now()
		st.EnsureIndex()
		ms := float64(time.Since(t0).Microseconds()) / 1000
		loads = append(loads, ms)
		t.Logf("run=%d index.load_ms=%.2f index_kib=%.1f", i+1, ms, float64(st.IndexBytes())/1024)
	}
	st.Close()
	sort.Float64s(loads)
	msg := fmt.Sprintf("RESULT median_load_ms=%.2f min=%.2f max=%.2f build_ms=%.2f retained_mib=%.2f sidecars=%d",
		loads[len(loads)/2], loads[0], loads[len(loads)-1], float64(build.Microseconds())/1000, float64(bytes)/(1<<20), nIdx)
	t.Log(msg)
	fmt.Println(msg)
}
