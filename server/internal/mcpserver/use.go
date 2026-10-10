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
// anything: the device ID to start (online) or, for a device that is not
// connected, its stored data (d).
func (t *Tools) target(name string) (id string, d *device.Device, err error) {
	if !t.M.Lazy {
		d, err := t.resolveDevice(name)
		return "", d, err
	}
	t.M.Track(device.CatchUpWait) // part of the single 10 s wait (§5)
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
			if len(identifying) > 0 {
				return "", nil, slow() // maybe it is that device
			}
			return "", sd, nil // stored data of a device that is not connected
		}
		if len(identifying) > 0 {
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
// A device that is not connected only serves its stored data.
func (t *Tools) use(name string) (d *device.Device, done func(), err error) {
	done = func() {}
	id, sd, err := t.target(name)
	if err != nil || id == "" {
		return sd, done, err
	}
	if err := t.M.Want(id, true); err != nil {
		var me *device.ErrMaxDevices
		if errors.As(err, &me) {
			return nil, done, errf("%v", err)
		}
		return nil, done, errf("starting %s: %v", id, err)
	}
	done = t.M.Begin(id)
	d = t.M.Get(id)
	if d == nil {
		done()
		return nil, func() {}, errf("device %s is connected but could not be opened for recording; see logcat_status", id)
	}
	return d, done, nil
}

// useAndWait is use plus the catch-up wait (§5): it returns the device
// and a backfill note ("" when caught up).
func (t *Tools) useAndWait(ctx context.Context, name string) (*device.Device, func(), string, error) {
	start := time.Now()
	d, done, err := t.use(name)
	if err != nil {
		return nil, done, "", err
	}
	p := d.WaitCaughtUp(ctx, waitLeft(start))
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
func waitLeft(start time.Time) time.Duration {
	return max(0, device.CatchUpWait-time.Since(start))
}
