package device

import (
	"sync"
	"testing"
	"time"
)

// Concurrent setState("device") calls start one recording session (issue
// #193): the "not running" check and the publication of d.cancel are one
// critical section. applyStates reaches setState from the track loop, from
// recheck and from Want with a snapshot taken outside any lock.
func TestConcurrentSetStateStartsOneSession(t *testing.T) {
	e := fakeEnv(t)
	addDevices(t, e, 5, "FAKEA")
	m, cancel := closeManager(t)
	defer func() { cancel(); m.Close() }()
	d := startDevice(t, m, "FAKEA")
	const callers = 8
	for round := 0; round < 6; round++ {
		d.setState(m.ctx, "") // stop
		d.mu.RLock()
		base := d.sessSeq
		d.mu.RUnlock()
		var ready, wg sync.WaitGroup
		release := make(chan struct{})
		d.mu.Lock() // every caller is parked on d.mu, then they all run at once
		for i := 0; i < callers; i++ {
			ready.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-release
				d.setState(m.ctx, "device")
			}()
		}
		ready.Wait()
		close(release)
		time.Sleep(20 * time.Millisecond) // callers reach d.mu
		d.mu.Unlock()
		wg.Wait()
		d.mu.RLock()
		got := d.sessSeq - base
		d.mu.RUnlock()
		if got != 1 {
			t.Fatalf("round %d: %d concurrent setState(device) started %d recording sessions, want 1", round, callers, got)
		}
	}
}
