package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// CarriedFiles are the per-device state files a recording takes along when
// it moves to another data root (set-root). Segments stay where they are.
var CarriedFiles = []string{"crashes.jsonl", "procs.jsonl", "dropbox_seen.jsonl", "packages.json"}

// ContinueIn prepares dst so that the recording of the (closed, flushed)
// store in src continues there (set-root, issue #3): meta.json (sessions, gaps, the logcat -T
// resume point, counters) and the aux files are copied, and the seqs
// continue after lastSeq (meta.SeqFloor), so cursors stay valid and
// monotonic and no seq is reused. If dst already holds segments of this
// device (the recording moves back to a root it used before), the seqs
// recorded elsewhere in between are noted as a hole. The first append in
// dst starts a new segment. boundary are the hashes of the lines stored at
// the resume timestamp (meta.ResumeDedup).
func ContinueIn(src, dst string, lastSeq uint64, boundary []uint64) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return errors.New("store: continue into the same directory")
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(src, "meta.json"))
	if err != nil {
		return err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	// what dst already has (a root used before)
	old, err := OpenReadOnly(dst, Config{})
	if err != nil {
		return err
	}
	dstLast := old.LastSeq()
	dstMeta := old.Meta()
	old.Close()
	if dstLast > lastSeq {
		lastSeq = dstLast // never reuse a seq
	}
	m.SeqFloor = lastSeq
	m.Holes = nil
	if dstLast > 0 {
		m.Holes = append(m.Holes, dstMeta.Holes...)
		if lastSeq > dstLast {
			m.Holes = append(m.Holes, SeqRange{First: dstLast + 1, Last: lastSeq})
		}
	}
	m.ResumeDedup = nil
	if len(boundary) > 0 && m.ArrivalNs() > 0 {
		m.ResumeDedup = &ResumeDedup{TimeNs: m.ArrivalNs(), Hashes: boundary}
	}
	m.LastSeenHostNs = time.Now().UnixNano()
	for _, name := range CarriedFiles {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	nb, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dst, "meta.json"), nb)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			os.Remove(dst) // stale copy from an earlier stay in this root
		}
		return err
	}
	return writeAtomic(dst, b)
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}
