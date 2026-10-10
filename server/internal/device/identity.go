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
}

// identity properties read by ProbeIdentity
var identityProps = []string{
	"ro.serialno", "ro.boot.serialno", "ro.kernel.qemu", "ro.boot.qemu",
	"ro.boot.qemu.avd_name", "ro.product.manufacturer", "ro.product.model",
	"ro.build.fingerprint",
}

func identityCmd() string {
	var b strings.Builder
	for _, p := range identityProps {
		fmt.Fprintf(&b, "echo \"%s=$(getprop %s)\"; ", p, p)
	}
	b.WriteString("echo \"android_id=$(settings get secure android_id 2>/dev/null)\"; ")
	b.WriteString("echo \"boot_id=$(cat /proc/sys/kernel/random/boot_id)\"")
	return b.String()
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
	out, err := adb.Shell(ctx, addr, 15*time.Second, identityCmd())
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
	return Identity{ID: id, Source: src, Addr: addr, BootID: props["boot_id"]}, nil
}
