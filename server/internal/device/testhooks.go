package device

import "time"

// HoldSessionEndForTest (tests of other packages only) keeps recording
// session sess of device serial from returning until the returned release
// is called, and shortens the wait of the next session for it to wait;
// release also restores that wait. It simulates a recorder that does not
// stop (issue #30).
func HoldSessionEndForTest(serial string, sess uint64, wait time.Duration) (release func()) {
	ch := make(chan struct{})
	f := func(d *Device, s uint64) {
		if d.Serial == serial && s == sess {
			<-ch
		}
	}
	old := recorderStopWait.Get()
	recorderStopWait.Set(wait)
	sessionEndHook.Store(&f)
	return func() {
		close(ch)
		sessionEndHook.Store(nil)
		recorderStopWait.Set(old)
	}
}
