package device

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func TestReplayWritesLaunches(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, "emu", store.Config{MaxAge: 24 * time.Hour, IdleRelease: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPipeline(st, "b1", time.UTC)
	text, err := os.ReadFile("../testdata/api29/session-raw.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ReplayText(string(text)); err != nil {
		t.Fatal(err)
	}
	aux := filepath.Join(st.Dir, "launches.jsonl")
	fi, err := os.Stat(aux)
	if err != nil {
		t.Fatalf("no launches.jsonl: %v", err)
	}
	n := len(LaunchSummaries(st))
	t.Logf("launches.jsonl size=%d summaries=%d", fi.Size(), n)
	if n == 0 {
		t.Fatal("no launches indexed after replay")
	}
}
