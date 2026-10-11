package device

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
)

// The recorder streams with `adb exec-out logcat ...` (raw stream: no pty,
// no shell-protocol framing, no CRLF translation; markedly faster than
// `adb logcat` / `adb shell logcat` on slow transports such as the
// emulator's). adbd supports exec since Android 5.0 (API 21); older
// devices, or an exec-out stream that fails before its first line, fall
// back to `adb logcat` (whose CRLF the reader strips).

// noExecOut remembers devices (by store directory) whose exec-out failed, for the
// lifetime of the process.
var noExecOut sync.Map

// streamArgs are the adb arguments of the logcat stream.
func streamArgs(addr, resume string, execOut bool) []string {
	a := []string{"-s", addr}
	if execOut {
		a = append(a, "exec-out")
	}
	return append(a, logcat.Args(resume)...)
}

// useExecOut reports whether to stream with exec-out.
func (d *Device) useExecOut() bool {
	if d.execOut.Load() < 0 {
		return false
	}
	if _, no := noExecOut.Load(d.Store.Dir); no {
		return false
	}
	if sdk, err := strconv.Atoi(strings.TrimSpace(d.Store.Meta().SDK)); err == nil && sdk > 0 && sdk < 21 {
		return false
	}
	return true
}

// execOutFailed decides, for an exec-out stream that ended without a
// line, whether exec-out itself failed (fall back to `adb logcat` for this
// device) rather than the device going away.
func (d *Device) execOutFailed(werr error, stderr string) bool {
	if werr == nil || d.execOut.Load() > 0 {
		return false // it worked before: a disconnect, not missing support
	}
	l := strings.ToLower(stderr)
	for _, gone := range []string{"not found", "offline", "unauthorized", "no devices", "no emulators", "device still"} {
		if strings.Contains(l, gone) {
			return false
		}
	}
	d.execOut.Store(-1)
	noExecOut.Store(d.Store.Dir, true)
	log.Printf("%s: adb exec-out logcat failed before its first line (%v %s); streaming with adb logcat instead", d.Serial, werr, firstLines(stderr, 2))
	return true
}

// dur is a duration knob that tests may change while recorders run.
type dur struct{ v atomic.Int64 }

func newDur(d time.Duration) *dur {
	x := &dur{}
	x.v.Store(int64(d))
	return x
}

func (x *dur) Get() time.Duration  { return time.Duration(x.v.Load()) }
func (x *dur) Set(d time.Duration) { x.v.Store(int64(d)) }
