package device

import (
	"strings"
	"testing"
)

func physical(serial string) map[string]string {
	return map[string]string{"ro.serialno": serial, "ro.product.manufacturer": "samsung", "ro.product.model": "SM-G991B",
		"ro.build.fingerprint": "samsung/o1sxxx/o1s:14/UP1A/1:user/release-keys", "android_id": "9774d56d682e549c"}
}

// The same phone over USB, Wi-Fi adb (ip:port) and mDNS adb maps to one ID.
func TestResolveIDUSBAndWirelessSameDevice(t *testing.T) {
	t.Parallel()
	p := physical("R58M123ABCD")
	var ids []string
	for _, addr := range []string{"R58M123ABCD", "192.168.1.20:5555", "adb-R58M123ABCD-AbCdEf._adb-tls-connect._tcp"} {
		id, src := ResolveID(addr, p, "")
		if src != "ro.serialno" {
			t.Errorf("%s: source %s", addr, src)
		}
		ids = append(ids, id)
	}
	if ids[0] != "R58M123ABCD" || ids[1] != ids[0] || ids[2] != ids[0] {
		t.Fatalf("ids %v", ids)
	}
}

// Two AVDs started one after the other on the same console port get
// different IDs (ro.serialno is the same on both).
func TestResolveIDTwoAVDsSamePort(t *testing.T) {
	t.Parallel()
	a := map[string]string{"ro.kernel.qemu": "1", "ro.serialno": "EMULATOR35X1X11X0", "ro.boot.qemu.avd_name": "Pixel_6_API_34"}
	b := map[string]string{"ro.kernel.qemu": "1", "ro.serialno": "EMULATOR35X1X11X0", "ro.boot.qemu.avd_name": "Pixel_3a_API_30"}
	ida, _ := ResolveID("emulator-5554", a, "")
	idb, _ := ResolveID("emulator-5554", b, "")
	if ida != "avd-Pixel_6_API_34" || idb != "avd-Pixel_3a_API_30" {
		t.Fatalf("ids %q %q", ida, idb)
	}
	// API <= 29: no avd_name property, the console tells the name
	old := map[string]string{"ro.kernel.qemu": "1", "ro.serialno": "EMULATOR37X2X12X0"}
	id1, src := ResolveID("emulator-5556", old, "api29")
	id2, _ := ResolveID("emulator-5556", old, "api28")
	if id1 != "avd-api29" || src != "emu_avd_name" || id2 == id1 {
		t.Fatalf("console names: %q (%s) %q", id1, src, id2)
	}
	// ro.boot.qemu (API 31+) also marks an emulator, whatever the address
	id3, _ := ResolveID("127.0.0.1:5555", map[string]string{"ro.boot.qemu": "1", "ro.boot.qemu.avd_name": "x"}, "")
	if id3 != "avd-x" {
		t.Fatalf("tcp emulator: %q", id3)
	}
}

func TestResolveIDFallback(t *testing.T) {
	t.Parallel()
	for _, serial := range []string{"", "unknown", "0123456789ABCDEF", "0000000000", "null"} {
		p := physical(serial)
		id, src := ResolveID("192.168.1.9:5555", p, "")
		if !strings.HasPrefix(id, "fp-") || len(id) != 15 || src != "android_id" {
			t.Errorf("serial %q: %q (%s)", serial, id, src)
		}
		// stable across transports (android_id based)
		if id2, _ := ResolveID("ABC", p, ""); id2 != id {
			t.Errorf("serial %q: fallback differs by address: %q vs %q", serial, id, id2)
		}
	}
	// another phone of the same model differs
	p1, p2 := physical(""), physical("")
	p2["android_id"] = "1111222233334444"
	if a, _ := ResolveID("x", p1, ""); a == func() string { b, _ := ResolveID("x", p2, ""); return b }() {
		t.Error("two devices with different android_id share an ID")
	}
	// no readable android_id: fingerprint + address (documented: not stable across transports)
	p := physical("unknown")
	p["android_id"] = "null"
	id, src := ResolveID("192.168.1.9:5555", p, "")
	if !strings.HasPrefix(id, "fp-") || src != "fingerprint+addr" {
		t.Errorf("no android_id: %q (%s)", id, src)
	}
	// emulator without an AVD name (API <= 29 over adb connect) falls back too
	if id, _ := ResolveID("127.0.0.1:5555", map[string]string{"ro.kernel.qemu": "1", "ro.serialno": "EMULATOR37X2X12X0", "android_id": "11bd2c287e04663c"}, ""); !strings.HasPrefix(id, "fp-") {
		t.Errorf("nameless emulator: %q", id)
	}
}

func TestParseIdentityOutput(t *testing.T) {
	t.Parallel()
	out := "ro.serialno=EMULATOR37X2X12X0\r\nro.kernel.qemu=1\nro.boot.qemu.avd_name=\nandroid_id=11bd2c287e04663c\nboot_id=ccb368e7-f2c9\n"
	p := ParseIdentityProps(out)
	if p["ro.serialno"] != "EMULATOR37X2X12X0" || p["ro.kernel.qemu"] != "1" || p["boot_id"] != "ccb368e7-f2c9" || p["ro.boot.qemu.avd_name"] != "" {
		t.Fatalf("%v", p)
	}
	if n := ParseAVDName("api29\r\nOK\r\n"); n != "api29" {
		t.Fatalf("avd name %q", n)
	}
	if n := ParseAVDName("KO: unknown command\r\n"); n != "" {
		t.Fatalf("avd name from error %q", n)
	}
	if !strings.Contains(identityCmd(), "boot_id") {
		t.Fatal("probe must read boot_id")
	}
}
