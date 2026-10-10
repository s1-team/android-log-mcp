package device

import (
	"log"
	"path/filepath"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/layout"
	"github.com/s1crab/android-log-mcp/server/internal/model"
	"github.com/s1crab/android-log-mcp/server/internal/store"
)

// Live root switch (issue #9). `android-log-mcp set-root` rewrites
// config.json; every process with FollowConfig re-reads it on each
// maintenance tick (every 10 s) and before taking ownership of a device.
// When the configured root differs from Base:
//
//   - each device this process records is moved: its recorder stops (the
//     logcat stream ends, so the current segment is finished and flushed),
//     the store is closed, its state (meta: sessions, gaps, the logcat -T
//     resume point, counters; aux files) is carried into <new root>/v1/<id>
//     with the seqs continuing (store.ContinueIn), and recording resumes
//     there in a new segment with logcat -T; the lines at the resume
//     timestamp that were already stored are deduplicated, so there is no
//     gap and no duplicate. The owner lock is held throughout (never two
//     writers) and is then rewritten with the new root and data dir: that
//     is the acknowledgement set-root waits for;
//   - devices followed read-only re-follow their owner's new dir as soon as
//     its owner lock names it (recheck / ownerMoved);
//   - read-only dirs of the old root are dropped and the new root's are
//     loaded.
//
// History from before the switch is not copied: it stays in the old root
// (set-root --keep-old) or is deleted (--delete-old).

// SyncRoot moves this process to the configured root if it changed.
func (m *Manager) SyncRoot() {
	if !m.FollowConfig || m.NoADB {
		return
	}
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	m.syncRootLocked()
}

func (m *Manager) syncRootLocked() {
	if !m.FollowConfig || m.NoADB {
		return
	}
	root, err := layout.ConfiguredRoot()
	if err != nil {
		log.Printf("config: %v (keeping data root %s)", err, m.Base)
		return
	}
	if filepath.Clean(root) == filepath.Clean(m.Base) {
		return
	}
	m.switchRootLocked(root)
}

func (m *Manager) switchRootLocked(root string) {
	old := m.Base
	log.Printf("data root changed (set-root): %s -> %s", old, root)
	nr := layout.Root{Dir: root, OwnersDir: m.Root.OwnersDir}
	if err := nr.Ensure(); err != nil {
		log.Printf("data root %s: %v (staying in %s)", root, err, old)
		return
	}
	m.Base, m.Root = root, nr
	for _, d := range m.Devices() {
		d.mu.RLock()
		f := d.foreign
		d.mu.RUnlock()
		switch {
		case d.Owned():
			m.moveDevice(d)
		case f != nil:
			// followers re-follow the owner's dir once it moves (recheck)
		default:
			m.mu.Lock()
			for k, e := range m.devices {
				if e == d {
					delete(m.devices, k)
				}
			}
			m.mu.Unlock()
			d.stop()
			go func(st *store.Store) { time.Sleep(time.Minute); st.Close() }(d.Store)
		}
	}
	m.loadDirs()
	m.Disk.Check(m.Base)
}

// moveDevice continues the recording of an owned device in the new root.
func (m *Manager) moveDevice(d *Device) {
	id := d.Serial
	src := d.Store.Dir
	dst := layout.DeviceDir(m.Base, id)
	d.mu.RLock()
	state := d.state
	d.mu.RUnlock()
	d.stopWait(10 * time.Second)
	seed := boundaryHashes(d.Store)
	last := d.Store.LastSeq()
	d.Store.Close() // flushes the current segment and meta
	reopen := func() *store.Store {
		st, err := store.OpenDir(src, id, m.Cfg, true)
		if err != nil {
			log.Printf("%s: reopen %s: %v", id, src, err)
		}
		return st
	}
	st := (*store.Store)(nil)
	if err := store.ContinueIn(src, dst, last); err != nil {
		log.Printf("%s: cannot move to %s: %v (recording stays in %s)", id, dst, err, src)
		st = reopen()
		dst = src
	} else if st, err = store.OpenDir(dst, id, m.Cfg, true); err != nil || !st.IsWriter() {
		log.Printf("%s: open %s as writer: %v (recording stays in %s)", id, dst, err, src)
		if st != nil {
			st.Close()
		}
		st = reopen()
		dst = src
	}
	if st == nil {
		return
	}
	d.mu.Lock()
	lock := d.owner
	d.owner = nil // handed over, not released by swap
	nd := m.newDevice(st, id)
	nd.addr, nd.owner, nd.adb, nd.idNote, nd.bootID = d.addr, lock, d.adb, d.idNote, d.bootID
	nd.loc, nd.tzName, nd.offset, nd.offsetAt = d.loc, d.tzName, d.offset, d.offsetAt
	d.mu.Unlock()
	if dst != src {
		nd.dedupSeed = seed
	}
	m.swap(id, nd)
	if dst != src {
		// the acknowledgement set-root waits for
		lock.Update(func(o *layout.Owner) { o.DataDir, o.Root = dst, m.Base })
		log.Printf("%s: recording continues in %s (seq %d onwards)", id, dst, last+1)
	}
	if state != "" && m.ctx != nil {
		nd.setState(m.ctx, state)
	}
}

// boundaryHashes are the hashes of the stored lines at the newest
// timestamp: logcat -T <that time> repeats them, and the new store does not
// have them to deduplicate against.
func boundaryHashes(st *store.Store) map[uint64]bool {
	last := st.LastSeq()
	ts := st.Meta().LastTimeNs
	out := map[uint64]bool{}
	if ts == 0 {
		return out
	}
	st.Scan(0, last+1, true, func(r *model.Record) bool {
		if r.TimeNs < ts {
			return false
		}
		out[recHash(r)] = true
		return true
	})
	return out
}
