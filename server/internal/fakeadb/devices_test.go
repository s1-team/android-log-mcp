package fakeadb

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #256: SetDevices replaces the device list (tmp file + rename) while
// the fake's track-devices reads it every 50 ms in another process. On
// Windows a read racing the rename failed and was taken as an empty list (a
// spurious disconnect), and the rename could fail while a reader had the
// file open. No read may see a missing or empty list, no replace may fail.
func TestDevicesListReplacedWhileRead(t *testing.T) {
	t.Parallel()
	e := Env{Dir: t.TempDir()}
	lists := []map[string]string{{"FAKEA": "device"}, {"FAKEA": "offline"}}
	if err := e.SetDevices(lists[0]); err != nil {
		t.Fatal(err)
	}
	var (
		wg         sync.WaitGroup
		stop       atomic.Bool
		reads, bad atomic.Int64
		writes     int
		writeErrs  int
		firstErr   error
	)
	// two readers polling every millisecond: far denser than track-devices'
	// 50 ms. Before the fix, Windows CI saw 116 of 17309 reads without a
	// list and 1875 of 1934 renames fail ("Access is denied") with 4
	// readers in a busy loop.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				reads.Add(1)
				if cur, ok := readDevices(e); !ok || cur == "" {
					bad.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); {
		for _, l := range lists {
			writes++
			if err := e.SetDevices(l); err != nil {
				writeErrs++
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("%d reads, %d replaces", reads.Load(), writes)
	if bad.Load() != 0 || writeErrs != 0 {
		t.Fatalf("%d of %d reads saw no device list; %d of %d replaces failed (first: %v)", bad.Load(), reads.Load(), writeErrs, writes, firstErr)
	}
}
