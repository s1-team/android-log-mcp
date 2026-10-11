package device

import "time"

// Dur is a duration that can be changed while the Manager runs (tests).
type Dur = dur

// Timing holds the device recording's time constants. NewManager uses
// DefaultTiming; tests shorten the ones they exercise on their own
// Manager instead of package globals, so they can run in parallel (#26).
type Timing struct {
	// BgStopWait bounds the wait for background work after cancelling
	// it; what is still running then is logged and its adb processes are
	// killed (#24).
	BgStopWait *Dur
	// AdbWaitDelay bounds how long a cancelled adb call waits for its
	// output pipes to close (a grandchild holding them must not hang the
	// caller).
	AdbWaitDelay *Dur
	// RecorderStopWait bounds the wait for a stopped recorder goroutine
	// (the last drain into the store, reaping logcat): release() and the
	// start of the next recording session (handOver).
	RecorderStopWait *Dur
	// SideWorkWait: side work starts at the latest this long after
	// recording started, even when the backfill has not caught up by then.
	SideWorkWait *Dur
	// PackagesRefreshEvery: the package list is re-read for a device
	// process whose application uid it does not contain (an app installed
	// after it was read) at most this often (issue #41).
	PackagesRefreshEvery *Dur
	// DropboxDelay lets AMS write a crash's DropBox entry before it is read.
	DropboxDelay *Dur
	// DropboxRecent: after catch-up, a tag fetched successfully this
	// recently is not fetched again for crashes found in the backfill.
	DropboxRecent *Dur
	// ProbeReuse: a probe this recent is reused when recording starts.
	ProbeReuse *Dur
	// ReconcileEvery is how often the device buffer is compared with the
	// stored lines in repair mode.
	ReconcileEvery *Dur
	// ReconcileSettle: a buffer read is compared this long after it was
	// taken, so lines the stream delivered meanwhile are stored (pipeline
	// Delay 800 ms).
	ReconcileSettle *Dur
	// ReleasePause (tests) delays release() after the recorder stopped,
	// before the store is reopened read-only and tracking stops: the window
	// a call can hit while a device is being stopped (slow on Windows;
	// issue #15).
	ReleasePause *Dur
}

// DefaultTiming returns the production time constants.
func DefaultTiming() *Timing {
	return &Timing{
		BgStopWait:           newDur(5 * time.Second),
		AdbWaitDelay:         newDur(2 * time.Second),
		RecorderStopWait:     newDur(10 * time.Second),
		SideWorkWait:         newDur(time.Minute),
		PackagesRefreshEvery: newDur(60 * time.Second),
		DropboxDelay:         newDur(4 * time.Second),
		DropboxRecent:        newDur(5 * time.Minute),
		ProbeReuse:           newDur(30 * time.Second),
		ReconcileEvery:       newDur(20 * time.Second),
		ReconcileSettle:      newDur(2 * time.Second),
		ReleasePause:         newDur(0),
	}
}

// defaultTiming is used by devices and adb clients made without a Manager.
var defaultTiming = DefaultTiming()

// timing is the Device's Timing (its Manager's).
func (d *Device) timing() *Timing {
	if d.tm != nil {
		return d.tm
	}
	return defaultTiming
}

func (a *ADB) timing() *Timing {
	if a != nil && a.Timing != nil {
		return a.Timing
	}
	return defaultTiming
}
