package device

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

func TestCarryOverAndUncarry(t *testing.T) {
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
	if m.SeqFloor != 3 || m.ResumeDedup == nil || m.ResumeDedup.TimeNs != 200 || len(m.ResumeDedup.Hashes) != 3 {
		// all lines within resumeSlack of the newest arrival (issue #52)
		t.Fatalf("meta: %+v", m)
	}
	Uncarry(cs)
	if _, err := os.Stat(cs[0].To); !os.IsNotExist(err) {
		t.Fatalf("uncarry left %s", cs[0].To)
	}
}
