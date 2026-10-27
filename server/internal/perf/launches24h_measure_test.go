package perf

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Run with ALM_MEASURE_LAUNCHES=1 to print query.launches_package_24h warm/cold ms.
func TestMeasureLaunchesPackage24h(t *testing.T) {
	if os.Getenv("ALM_MEASURE_LAUNCHES") == "" {
		t.Skip("set ALM_MEASURE_LAUNCHES=1")
	}
	copies := defaultCopies
	r := New()
	base, _, _ := largeDataset(t, r, copies)
	tools, m := openTools(t, base, store.Config{})
	d := m.Get(fxSerial)
	if d == nil {
		t.Fatal("no device")
	}
	args := `{"package":"com.example.logtest","from":"24h","limit":20}`
	d.Store.ReleaseCaches()
	t0 := time.Now()
	out := callOK(t, tools, "logcat_app_launches", args)
	cold := time.Since(t0)
	var runs []float64
	for i := 0; i < 5; i++ {
		t0 = time.Now()
		out = callOK(t, tools, "logcat_app_launches", args)
		runs = append(runs, float64(time.Since(t0).Microseconds())/1000)
	}
	warm := median(runs)
	head := out
	if len(head) > 160 {
		head = head[:160]
	}
	fmt.Fprintf(os.Stderr, "PERF query.launches_package_24h cold_ms=%.2f warm_ms=%.2f bytes=%d copies=%d\nHEAD %s\n",
		float64(cold.Microseconds())/1000, warm, len(out), copies, head)
}
