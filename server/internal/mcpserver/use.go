package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
)

// Pins are the explicit recording pins (`record --for`,
// logcat_status(record_for)), implemented by the service.
type Pins interface {
	// Set pins device id for d (deadline = max(current, now + d)) and
	// returns the deadline.
	Set(id string, d time.Duration, by string) (time.Time, error)
	// Stop removes the device's pin and stops its recording now.
	Stop(id string) error
	// Until is the device's active pin deadline (zero: none).
	Until(id string) time.Time
	// Describe is a one-line description of the device's pin (with a
	// suspension and its last error), "" if it has none.
	Describe(id string, loc *time.Location) string
}

// MaxPin is the longest pin (issue #11 §8).
const MaxPin = 24 * time.Hour

var pinRe = regexp.MustCompile(`^(\d+h)?(\d+m)?(\d+s)?$`)

// ParsePin parses a pin duration: "0" (stop) or a Go duration with units,
// like 30m, 2h, 90m, 1h30m; at most 24h.
func ParsePin(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if s == "" || !pinRe.MatchString(s) {
		return 0, fmt.Errorf("record_for %q: expected a duration with a unit like 30m, 2h or 1h30m (or \"0\" to stop)", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("record_for %q: expected a positive duration like 30m, 2h or 1h30m (or \"0\" to stop)", s)
	}
	if d > MaxPin {
		return 0, fmt.Errorf("record_for %q is longer than the limit of 24h; pin at most 24h and extend it later", s)
	}
	return d, nil
}

// errSeveral: several devices are online and the call named none.
type errSeveral struct{ ids []device.Identity }

func (e *errSeveral) Error() string {
	var names []string
	for _, i := range e.ids {
		n := i.ID
		if i.Addr != "" && i.Addr != i.ID {
			n += " (adb " + i.Addr + ")"
		}
		names = append(names, n)
	}
	return fmt.Sprintf("%d devices are connected; pass device=one of: %s.", len(e.ids), strings.Join(names, ", "))
}

// target resolves the device a log-tool call is about, without starting
// anything: the device ID to start (online, or named by stored data while
// the device list or an identity is still pending) or, for a device that is
// known not to be connected, its stored data (d).
func (t *Tools) target(name string) (id string, d *device.Device, err error) {
	if !t.M.Lazy {
		d, err := t.resolveDevice(name)
		return "", d, err
	}
	t.M.Track(t.M.CatchUpWait) // part of the single 10 s wait (§5)
	online := t.M.OnlineIDs()
	identifying := t.M.Identifying()
	slow := func() error {
		return errf("adb lists %s but reading its identity has not finished yet (slow device); call again in a few seconds", strings.Join(identifying, ", "))
	}
	if name != "" {
		for _, i := range online {
			if i.ID == name || i.Addr == name {
				return i.ID, nil, nil
			}
		}
		if sd := t.M.Get(name); sd != nil {
			for _, i := range online {
				if i.ID == sd.Serial {
					return i.ID, nil, nil
				}
			}
			if len(identifying) > 0 || t.M.Settling() {
				// Not known to be offline: the first device list or an identity
				// is still pending, so it may be that device. Start it; it
				// attaches as soon as it is online (issue #109). Only a name that
				// matches stored data gets here, never a typo or foreign serial.
				return sd.Serial, nil, nil
			}
			return "", sd, nil // stored data of a device that is not connected
		}
		if len(identifying) > 0 {
			// the name may be the adb serial of a device whose identity is
			// still being read: start it as soon as that is known (issue #109)
			t.M.WantAddr(name)
			return "", nil, slow()
		}
		d, err := t.resolveDevice(name)
		return "", d, err
	}
	if len(identifying) > 0 && len(online) < 2 {
		return "", nil, slow() // none or one known: the others may make it several
	}
	switch len(online) {
	case 1:
		return online[0].ID, nil, nil
	case 0:
		d, err := t.resolveDevice("")
		return "", d, err
	}
	return "", nil, &errSeveral{online}
}

// use resolves the device of a log-tool call and starts recording it (a
// call refreshes its idle timer and keeps it busy until done is called).
// A device that is not connected only serves its stored data. It waits
// (within the 10 s budget from start) until a started device actually
// records: a call during an idle stop must not be answered from the old
// read-only device (issue #15); starting reports that it still did not.
func (t *Tools) use(name string, start time.Time) (d *device.Device, done func(), starting bool, err error) {
	done = func() {}
	id, sd, err := t.target(name)
	if err != nil || id == "" {
		return sd, done, false, err
	}
	if err := t.M.Want(id, true); err != nil {
		var me *device.ErrMaxDevices
		if errors.As(err, &me) {
			return nil, done, false, errf("%v", err)
		}
		return nil, done, false, errf("starting %s: %v", id, err)
	}
	done = t.M.Begin(id)
	d, ok := t.M.WaitRecording(context.Background(), id, t.waitLeft(start))
	if d == nil {
		done()
		return nil, func() {}, false, errf("device %s is connected but could not be opened for recording; see logcat_status", id)
	}
	return d, done, !ok, nil
}

// useAndWait is use plus the catch-up wait (§5): it returns the device
// and a backfill note ("" when caught up).
func (t *Tools) useAndWait(ctx context.Context, name string) (*device.Device, func(), string, error) {
	start := time.Now()
	d, done, starting, err := t.use(name, start)
	if err != nil {
		return nil, done, "", err
	}
	p := device.StartingProgress()
	if !starting {
		p = d.WaitCaughtUp(ctx, t.waitLeft(start))
	}
	if !p.Pending {
		return d, done, "", nil
	}
	return d, done, p.BackfillText(d.Loc()) + "; results may be incomplete, call again", nil
}

// Target resolves a device name like the tools do (the single online
// device for ""), starting nothing: an online device's ID, or the stored
// data of a device that is not connected.
func (t *Tools) Target(name string) (string, *device.Device, error) { return t.target(name) }

// waitLeft is what is left of the single catch-up wait (§5) of a call that
// began at start (starting tracking and identifying devices count).
func (t *Tools) waitLeft(start time.Time) time.Duration {
	return max(0, t.M.CatchUpWait-time.Since(start))
}
