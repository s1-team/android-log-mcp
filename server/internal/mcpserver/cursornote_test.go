package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// #173 G8: the cursor_pruned note says which seq "oldest" is.
func TestCursorPrunedNoteNamesFirstSeq(t *testing.T) {
	base := t.TempDir()
	cfg := store.Config{SegmentBytes: 3000, MemRecords: 20}
	st, err := store.Open(base, "emu-x", cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.Serial, m.TZ = "emu-x", "UTC"
		m.Sessions = append(m.Sessions, store.Session{BootID: "b1-full", Boot: "b1", FirstSeq: 1})
		m.LastBootID = "b1-full"
	})
	now := time.Now()
	var recs []*model.Record
	for i := 0; i < 200; i++ {
		recs = append(recs, &model.Record{TimeNs: now.Add(time.Duration(i-300) * time.Second).UnixNano(), PID: 1, TID: 1, Level: model.Info, Tag: "T", Msg: fmt.Sprintf("line %d", i), Boot: "b1"})
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		segs := store.ListSegments(st.Dir)
		if _, ok := st.RemoveOldestSegment(segs[0].Path); !ok {
			t.Fatal("remove oldest")
		}
	}
	first := st.FirstSeq()
	if first <= 2 {
		t.Fatalf("setup: first seq %d", first)
	}
	st.Close()
	m := device.NewManager(base, cfg)
	m.NoADB = true
	m.Start(context.Background())
	t.Cleanup(m.Close)
	tools := &Tools{M: m}
	cur := store.Cursor{Serial: "emu-x", Boot: "b1", Seq: 1}.String()
	out, isErr := tools.Call(context.Background(), "logcat_search", json.RawMessage(`{"after":"`+cur+`"}`))
	if isErr {
		t.Fatal(out)
	}
	want := fmt.Sprintf("note code=cursor_pruned boot=b1 first=%d using=oldest", first)
	if !strings.Contains(out, want) {
		t.Fatalf("want %q in:\n%s", want, out)
	}
}
