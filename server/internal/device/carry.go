package device

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Carried is one device directory carried over by CarryOver.
type Carried struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	LastSeq uint64 `json:"last_seq"`
	// Existed: To was there before (a root used earlier); its previous
	// state files are in Backup for a rollback.
	Existed bool              `json:"existed,omitempty"`
	Backup  map[string][]byte `json:"-"`
}

// CarryOver prepares newRoot so that the recording of every device in
// oldRoot continues there (set-root, issue #3): meta (sessions, gaps, the
// logcat -T resume point and the boundary lines to skip) and the aux files
// are copied and the seqs continue (store.ContinueIn). The stores must not
// be open for writing (the service has stopped recording). Segments stay
// in oldRoot. On error, the devices done so far are returned so that the
// caller can undo them (Uncarry).
func CarryOver(oldRoot, newRoot string) ([]Carried, error) {
	var out []Carried
	for _, dir := range store.ListDeviceDirs(oldRoot) {
		if store.WriterLockHeld(dir) {
			return out, fmt.Errorf("%s is being written by another process (writer.lock, pid %s), e.g. an older android-log-mcp; stop it first", dir, readWriterPID(dir))
		}
		st, err := store.OpenReadOnly(dir, store.Config{})
		if err != nil {
			return out, fmt.Errorf("%s: %v", dir, err)
		}
		c := Carried{ID: filepath.Base(dir), From: dir, To: filepath.Join(store.DataDir(newRoot), filepath.Base(dir)), LastSeq: st.LastSeq()}
		boundary := boundaryHashes(st)
		st.Close()
		if _, err := os.Stat(c.To); err == nil {
			c.Existed = true
			c.Backup = map[string][]byte{}
			for _, name := range append([]string{"meta.json"}, store.CarriedFiles...) {
				if b, err := os.ReadFile(filepath.Join(c.To, name)); err == nil {
					c.Backup[name] = b
				}
			}
		}
		out = append(out, c)
		if err := store.ContinueIn(c.From, c.To, c.LastSeq, boundary); err != nil {
			return out, fmt.Errorf("%s -> %s: %v", c.From, c.To, err)
		}
	}
	return out, nil
}

// Uncarry undoes CarryOver (rollback of a failed set-root).
func Uncarry(cs []Carried) {
	for _, c := range cs {
		if !c.Existed {
			os.RemoveAll(c.To)
			continue
		}
		for _, name := range append([]string{"meta.json"}, store.CarriedFiles...) {
			p := filepath.Join(c.To, name)
			if b, ok := c.Backup[name]; ok {
				os.WriteFile(p, b, 0o644)
			} else {
				os.Remove(p)
			}
		}
	}
}

// boundaryHashes are the hashes of the stored lines that logcat -T
// <newest arrival> can repeat (see boundaryMultiset); the new root does
// not have them to deduplicate against.
func boundaryHashes(st *store.Store) []uint64 {
	m := st.Meta()
	var out []uint64
	for h, n := range boundaryMultiset(st, m.ArrivalNs()) {
		for ; n > 0; n-- {
			out = append(out, h)
		}
	}
	return out
}
