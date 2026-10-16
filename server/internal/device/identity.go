package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Identity is the stable identity of a device, independent of how adb
// reaches it (USB serial, ip:port, mDNS name, emulator console port).
type Identity struct {
	ID     string // directory / owner-lock / tool key
	Source string // how ID was derived (see ResolveID)
	Addr   string // adb serial it was probed through
	BootID string
	// Probe is the recorder probe that came with the identity probe (one
	// adb shell for both); the recorder uses it instead of probing again
	// when it starts within Timing.ProbeReuse of it.
	Probe *Probe
	// Collision explains a disambiguated ID (another connected device
	// already uses the plain ID); empty normally.
	Collision string
}

// identity properties read by ProbeIdentity
var identityProps = []string{
	"ro.serialno", "ro.boot.serialno", "ro.kernel.qemu", "ro.boot.qemu",
	"ro.boot.qemu.avd_name", "ro.product.manufacturer", "ro.product.model",
	"ro.build.fingerprint",
}

// probe properties read by the recorder (and with the identity probe)
var probeProps = []string{
	"ro.product.model", "ro.build.version.release", "ro.build.version.sdk", "persist.sys.timezone",
}

// probeCmd is the recorder probe: device properties, zone, boot id and the
// device clock, as key=value lines. With identity it also reads the
// identity properties (ProbeIdentity), so one adb shell serves both.
// The clock is read before the slow `settings` call and again after it
// (now / now2), so the time spent in `settings` can be left out of the
// round trip the clock offset is centred on.
func probeCmd(identity bool) string {
	var b strings.Builder
	seen := map[string]bool{}
	props := probeProps
	if identity {
		props = append(append([]string{}, identityProps...), probeProps...)
	}
	for _, p := range props {
		if !seen[p] {
			seen[p] = true
			fmt.Fprintf(&b, "echo \"%s=$(getprop %s)\"; ", p, p)
		}
	}
	b.WriteString("echo \"zone=$(date +%z)\"; ")
	b.WriteString("echo \"boot_id=$(cat /proc/sys/kernel/random/boot_id)\"; ")
	b.WriteString("echo \"now=$(date +%s.%N)\"")
	if identity {
		b.WriteString("; echo \"android_id=$(settings get secure android_id 2>/dev/null)\"; ")
		b.WriteString("echo \"now2=$(date +%s.%N)\"")
	}
	return b.String()
}

func identityCmd() string { return probeCmd(true) }

// Probe is a parsed recorder probe (probeCmd) with the host times around
// the adb call.
type Probe struct {
	Props  map[string]string
	T0, T1 time.Time
}

// Clock returns the device clock reading, whether it has whole-second
// precision only, and the host time it corresponds to (the middle of the
// round trip without the time spent between now and now2).
func (p *Probe) Clock() (dev time.Time, coarse bool, mid time.Time, ok bool) {
	dev, coarse, ok = parseDate(p.Props["now"])
	if !ok {
		return dev, coarse, mid, false
	}
	rtt := p.T1.Sub(p.T0)
	if now2, c2, ok2 := parseDate(p.Props["now2"]); ok2 && !coarse && !c2 {
		if spent := now2.Sub(dev); spent > 0 && spent < rtt {
			rtt -= spent
		}
	}
	return dev, coarse, p.T0.Add(rtt / 2), true
}

// probeDeviceNow is the device time the probe stands for: its device clock
// reading, aged by the time since the probe when it was saved for reuse.
// Without a device clock reading it falls back to host now.
func probeDeviceNow(p *Probe, reused bool) time.Time {
	now, _, mid, ok := p.Clock()
	if !ok {
		return time.Now()
	}
	if reused {
		now = now.Add(time.Since(mid))
	}
	return now
}

// ParseIdentityProps parses the key=value output of the identity probe.
func ParseIdentityProps(out string) map[string]string {
	props := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props
}

// IsEmulator reports whether the device is an Android emulator.
func IsEmulator(addr string, props map[string]string) bool {
	return props["ro.kernel.qemu"] == "1" || props["ro.boot.qemu"] == "1" || strings.HasPrefix(addr, "emulator-")
}

var genericSerials = map[string]bool{
	"": true, "unknown": true, "0123456789abcdef": true, "0123456789": true,
	"1234567890abcdef": true, "abcdef0123456789": true, "null": true,
	"none": true, "serial": true, "android": true,
}

func allSame(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return len(s) > 1
}

// GenericSerial reports whether a ro.serialno value is missing or a known
// placeholder shared by many devices (cheap boards, some ROMs).
func GenericSerial(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return genericSerials[l] || allSame(l)
}

var hexID = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)

// ResolveID derives the stable device ID:
//
//   - emulator (ro.kernel.qemu / ro.boot.qemu = 1, or adb serial emulator-N):
//     "avd-<name>" from ro.boot.qemu.avd_name (API 30+) or the console's
//     `adb emu avd name` (avdName); ro.serialno is identical on many AVDs
//     and the console port is reused, so neither is used;
//   - physical device: ro.serialno (the USB serial; the same over Wi-Fi
//     adb, whatever ip:port or mDNS name adb shows), unless it is empty or
//     a generic placeholder;
//   - otherwise "fp-<12 hex>": sha256 of manufacturer|model|android_id, or —
//     when android_id is unreadable — manufacturer|model|fingerprint|addr
//     (then the ID follows the adb address, i.e. it is not stable across
//     transports).
func ResolveID(addr string, props map[string]string, avdName string) (id, source string) {
	if IsEmulator(addr, props) {
		name := props["ro.boot.qemu.avd_name"]
		src := "avd_name"
		if name == "" {
			name, src = strings.TrimSpace(avdName), "emu_avd_name"
		}
		if name != "" {
			return "avd-" + name, src
		}
	} else {
		for _, k := range []string{"ro.serialno", "ro.boot.serialno"} {
			if s := props[k]; !GenericSerial(s) {
				return s, k
			}
		}
	}
	h := sha256.New()
	man, model := props["ro.product.manufacturer"], props["ro.product.model"]
	if aid := props["android_id"]; hexID.MatchString(aid) {
		fmt.Fprintf(h, "%s|%s|%s", man, model, strings.ToLower(aid))
		return "fp-" + hex.EncodeToString(h.Sum(nil))[:12], "android_id"
	}
	fmt.Fprintf(h, "%s|%s|%s|%s", man, model, props["ro.build.fingerprint"], addr)
	return "fp-" + hex.EncodeToString(h.Sum(nil))[:12], "fingerprint+addr"
}

// ParseAVDName parses `adb emu avd name` output ("api29\r\nOK").
func ParseAVDName(out string) string {
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && l != "OK" && !strings.HasPrefix(l, "KO") && !strings.Contains(l, "error") {
			return l
		}
	}
	return ""
}

// ProbeIdentity reads the identity properties (one read-only adb shell) and,
// for an emulator without ro.boot.qemu.avd_name, asks the emulator console
// for the AVD name.
func ProbeIdentity(ctx context.Context, adb *ADB, addr string) (Identity, error) {
	t0 := time.Now()
	out, err := adb.Shell(ctx, addr, 15*time.Second, identityCmd())
	t1 := time.Now()
	if err != nil {
		return Identity{}, err
	}
	props := ParseIdentityProps(out)
	if props["boot_id"] == "" && props["ro.product.model"] == "" {
		return Identity{}, fmt.Errorf("identity probe: unexpected output %q", strings.TrimSpace(out))
	}
	avd := ""
	if IsEmulator(addr, props) && props["ro.boot.qemu.avd_name"] == "" && strings.HasPrefix(addr, "emulator-") {
		if o, err := adb.Output(ctx, 10*time.Second, "-s", addr, "emu", "avd", "name"); err == nil {
			avd = ParseAVDName(o)
		}
	}
	id, src := ResolveID(addr, props, avd)
	return Identity{ID: id, Source: src, Addr: addr, BootID: props["boot_id"], Probe: &Probe{Props: props, T0: t0, T1: t1}}, nil
}
