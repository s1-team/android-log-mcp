package fakeadb

import (
	"fmt"
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
		readErr    atomic.Value
		writes     int
		writeErrs  int
		firstErr   error
	)
	// Two readers poll every millisecond (track-devices: 50 ms) while the
	// list is replaced every 10 ms. Before the fix, with 4 readers and the
	// writer in busy loops, Windows CI saw 116 of 17309 reads without a list
	// and 1875 of 1934 renames fail ("Access is denied").
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				reads.Add(1)
				if cur, err := readDevices(e); err != nil || cur == "" {
					if bad.Add(1) == 1 {
						readErr.Store(fmt.Sprintf("%q, %v", cur, err))
					}
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
			time.Sleep(10 * time.Millisecond) // tests change the list now and then, not in a busy loop
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("%d reads, %d replaces", reads.Load(), writes)
	if bad.Load() != 0 || writeErrs != 0 {
		t.Fatalf("%d of %d reads saw no device list (first: %v); %d of %d replaces failed (first: %v)", bad.Load(), reads.Load(), readErr.Load(), writeErrs, writes, firstErr)
	}
}
