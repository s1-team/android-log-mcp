package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// CarriedFiles are the per-device state files a recording takes along when
// it moves to another data root (set-root). Segments stay where they are.
var CarriedFiles = []string{"crashes.jsonl", "procs.jsonl", "dropbox_seen.jsonl", "packages.json"}

// ResumeDedupFile holds the hashes carried by a set-root move: 8 bytes per
// line, little-endian, at most MaxResumeDedup of them (issue #56).
const ResumeDedupFile = "resume_dedup.bin"

// MaxResumeDedup bounds the carried hashes (4 MB file).
const MaxResumeDedup = 500000

// ContinueIn prepares dst so that the recording of the (closed, flushed)
// store in src continues there (set-root, issue #3): meta.json (sessions, gaps, the logcat -T
// resume point, counters) and the aux files are copied, and the seqs
// continue after lastSeq (meta.SeqFloor), so cursors stay valid and
// monotonic and no seq is reused. If dst already holds segments of this
// device (the recording moves back to a root it used before), the seqs
// recorded elsewhere in between are noted as a hole. The first append in
// dst starts a new segment. carried are the hashes of the newest stored lines
// of the current boot (resume_dedup.bin, meta.ResumeDedup), which a resume
// in dst can repeat.
func ContinueIn(src, dst string, lastSeq uint64, carried []uint64) error {
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
	os.Remove(filepath.Join(dst, ResumeDedupFile)) // stale file from an earlier stay in this root
	if len(carried) > MaxResumeDedup {
		carried = carried[len(carried)-MaxResumeDedup:]
	}
	if len(carried) > 0 && m.LastBootID != "" {
		buf := make([]byte, 8*len(carried))
		for i, h := range carried {
			binary.LittleEndian.PutUint64(buf[8*i:], h)
		}
		if err := writeAtomic(filepath.Join(dst, ResumeDedupFile), buf); err != nil {
			return err
		}
		m.ResumeDedup = &ResumeDedup{BootID: m.LastBootID, SeqFloor: lastSeq, Count: len(carried)}
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

// LoadResumeDedup returns the hashes a set-root move carried into this
// directory: resume_dedup.bin, or the legacy inline set of meta.json.
func (s *Store) LoadResumeDedup() []uint64 {
	m := s.Meta()
	rd := m.ResumeDedup
	if rd == nil {
		return nil
	}
	if rd.Count == 0 {
		return rd.Hashes
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, ResumeDedupFile))
	if err != nil || len(b) != 8*rd.Count {
		return nil // missing or damaged: de-dup falls back to the stored lines
	}
	out := make([]uint64, rd.Count)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(b[8*i:])
	}
	return out
}

// ClearResumeDedup drops the carried set (writer only): the file and the
// meta field.
func (s *Store) ClearResumeDedup() {
	os.Remove(filepath.Join(s.Dir, ResumeDedupFile))
	s.UpdateMeta(func(m *Meta) { m.ResumeDedup = nil })
}
