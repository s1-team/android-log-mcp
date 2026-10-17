package device

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func TestCarryOverAndUncarry(t *testing.T) {
	t.Parallel()
	oldRoot, newRoot := t.TempDir(), filepath.Join(t.TempDir(), "new")
	st, err := store.Open(oldRoot, "dev", store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) {
		m.LastBootID = "b"
		m.Sessions = []store.Session{{BootID: "b", Boot: "b", FirstSeq: 1}}
	})
	recs := []*model.Record{
		{TimeNs: 100, Level: 'I', Tag: "A", Msg: "1", Boot: "b"},
		{TimeNs: 200, Level: 'I', Tag: "A", Msg: "2", Boot: "b"},
		{TimeNs: 200, Level: 'I', Tag: "A", Msg: "3", Boot: "b"},
	}
	if err := st.Append(recs); err != nil {
		t.Fatal(err)
	}
	st.UpdateMeta(func(m *store.Meta) { m.LastTimeNs = 200 })
	st.Close()
	cs, err := CarryOver(oldRoot, newRoot)
	if err != nil || len(cs) != 1 || cs[0].LastSeq != 3 || cs[0].Existed {
		t.Fatalf("carry: %+v %v", cs, err)
	}
	nst, err := store.OpenReadOnly(cs[0].To, store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m := nst.Meta()
	nst.Close()
	rd := m.ResumeDedup
	if m.SeqFloor != 3 || rd == nil || rd.Count != 4 || rd.SeqFloor != 3 || rd.BootID != "b" || len(rd.Hashes) != 0 {
		// the last lines of the boot session by seq, in resume_dedup.bin (issue #56),
		// plus the whole-entry hash of the two same-header records 2 and 3 (issue #192)
		t.Fatalf("meta: %+v", m)
	}
	if b, err := os.ReadFile(filepath.Join(cs[0].To, store.ResumeDedupFile)); err != nil || len(b) != 32 {
		t.Fatalf("%s: %d bytes, %v", store.ResumeDedupFile, len(b), err)
	}
	Uncarry(cs)
	if _, err := os.Stat(cs[0].To); !os.IsNotExist(err) {
		t.Fatalf("uncarry left %s", cs[0].To)
	}
}
