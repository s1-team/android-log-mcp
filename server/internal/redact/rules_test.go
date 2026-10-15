package redact

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/logcat"
	"github.com/s1crab/android-log-mcp/server/internal/testhome"
)

func TestCookie(t *testing.T) {
	r := newT(t, Options{})
	out := positive(t, r, "cookie", "--> Cookie: SESSIONID=8f2a9c77d1e4; theme=dark", "8f2a9c77d1e4")
	if !strings.HasPrefix(out, "--> Cookie: ") {
		t.Errorf("header name damaged: %q", out)
	}
	positive(t, r, "cookie", `set-cookie="sid=a1b2c3d4e5f6; Path=/; HttpOnly"`, "a1b2c3d4e5f6")
	negative(t, r, "CookieManager: cookies enabled")
	negative(t, r, "cookie: null")
}

func TestDeviceIDs(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "device_id", "SubInfo:{id=1, iccId=89014103211118510720 simSlotIndex=0", "89014103211118510720")
	positive(t, r, "device_id", "imei: 490154203237518 slot 0", "490154203237518")
	positive(t, r, "device_id", "android_id=9774d56d682e549c", "9774d56d682e549c")
	negative(t, r, "serial=unknown")
	negative(t, r, "imei not available")
}

func TestCredentialAfterColonNeedsOpaqueValue(t *testing.T) {
	r := newT(t, Options{})
	negative(t, r, "removeWindowToken: Attempted to remove non-existing token: android.os.Binder@8160d95")
	negative(t, r, "wm_task_removed: [5,removeAppToken: last token]")
	negative(t, r, "password: required")
	positive(t, r, "credential", "auth_token: 9f8e7d6c5b4a3f2e", "9f8e7d6c5b4a3f2e")
	positive(t, r, "credential", `password: "correct horse"`, "correct horse")
	positive(t, r, "credential", "password=hunter2", "hunter2")
}

func TestPhoneContexts(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "phone", "contact +14155552671 now", "+14155552671")
	positive(t, r, "phone", "mobile:13912345678", "13912345678")
	positive(t, r, "phone", "SubInfo mNumber=15555215554 dataRoaming=0", "15555215554")
	negative(t, r, "rawOffset=+28800000 dst=0")                                                         // tz offset in ms
	negative(t, r, "totalBytes=13958643712 free=13958643712")                                           // sizes
	negative(t, r, "elapsed 15000000000 ns")                                                            // unit
	negative(t, r, "durationMs=15000000000")                                                            // camelCase unit key
	negative(t, r, "| state=S schedstat=( 9487166493 15127253939 2611 ) utm=561 stm=387 core=3 HZ=100") // counters
	negative(t, r, "| state=S schedstat=( 13812345678 149155943855 5405 ) utm=1287")
}

func TestBankCardContexts(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "bank_card", "card=4111111111111111", "4111111111111111")
	positive(t, r, "bank_card", "amex 378282246310005", "378282246310005")
	positive(t, r, "bank_card", "pan: 5555-5555-5555-4444", "5555-5555-5555-4444")
	// real line from the emulator: frame timestamps in ns are 13 digits, some pass Luhn
	negative(t, r, "Davey! duration=1471ms; Flags=0, IntendedVsync=6354998857553, Vsync=6355015524219, OldestInputEvent=9223372036854775807, NewestInputEvent=0, HandleInputStart=6355018932013, AnimationStart=6355675536698")
	negative(t, r, "IntendedVsync=4111111111111111 Vsync=4111111111111129")            // a counter key, not a card key
	negative(t, r, "x0  4111111111111111  x1  0000000000000000  x2  0000007fe1b2c3d0") // register dump
	negative(t, r, "    #03 pc 4111111111111111  /data/app/com.example/lib/x86_64/liblogtest.so")
	negative(t, r, "    0000007fe1b2c3d0 4111111111111111  [anon:libc_malloc]") // memory dump
	negative(t, r, "4111111111111")                                             // 13 bare digits: too weak
}

func TestIPVersusVersions(t *testing.T) {
	r := newT(t, Options{})
	negative(t, r, "Radio: 1.0.0.0")
	negative(t, r, "version 1.2.3.4 build 5")
	negative(t, r, "versionName=10.0.2.15")
	negative(t, r, "Mozilla/5.0 Chrome/83.0.41.10 Mobile")
	negative(t, r, "loaded v1.2.3.4")
	positive(t, r, "ipv4", "failed to connect to www.google.com/198.18.0.1 (port 80)", "198.18.0.1")
	positive(t, r, "ipv4", "GET http://203.0.113.9/x", "203.0.113.9")
	positive(t, r, "ipv6", "route ::/0 -> fe80::2 radio0", "fe80::2")
}

func TestEmailVersusLibraries(t *testing.T) {
	r := newT(t, Options{})
	negative(t, r, "00007ef7'cb31c000-00007ef7'cb31cfff r-- 0 1000 /system/lib64/vndk-sp-29/android.hardware.graphics.mapper@2.0.so (BuildId: c82e5b89)")
	negative(t, r, "/data/resource-cache/product@overlay@DisplayCutoutEmulationCorner@DisplayCutoutEmulationCornerOverlay.apk@idmap")
	negative(t, r, "native: #04 pc 000000000000f378  /system/lib64/android.hardware.sensors@1.0.so (BpHwSensors::_hidl_poll)")
	positive(t, r, "email", "login ok for dev.ops@corp.example.com", "dev.ops@corp.example.com")
}

// Explicit negatives for every structural element the issue lists:
// timestamps, pid/tid, version numbers, hex values/addresses, stack frames.
func TestStructuralNegatives(t *testing.T) {
	r := newT(t, Options{})
	for _, s := range []string{
		// timestamps
		"2026-10-10 03:05:52.878 to 2026-10-10 03:06:10.586",
		"at 1791572202.589379 / 1791572202589 ms / 1791572202589379 us / 1791572202589379000 ns",
		"uptime 6355675536698 realtime 6355675536698",
		"10-10 03:02:39.901  6713  6713 V LogTest : x",
		// pid / tid / uid
		"pid 13812 tid 13813 uid 10146",
		"Process com.example.logtest (pid 13812) has died: fg  TOP",
		"pid: 13812345, tid: 13812346, name: RenderThread  >>> com.example.logtest <<<",
		"am_proc_died: [0,13812,com.example.logtest,0,2]",
		// version numbers
		"versionCode=139123456 versionName=13.9.1.2",
		"Build fingerprint: 'google/sdk_gphone_x86_64/generic_x86_64:10/QSR1.211112.011/13135432:userdebug/dev-keys'",
		"Android SDK 29 (10.0.0) kernel 4.14.112+ build 6355675",
		// hex values / addresses
		"fault addr 0x7ffd5b8e1a20 sp 0x13812345678",
		"rax 0000000000000000  rbx 00007ef862cd4000  rcx 4111111111111111  rdx 0000000000000004",
		"memory near rbx: 00007ef862cd4000 4111111111111111 13812345678abcde",
		"BuildId: 1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d",
		"android.os.BinderProxy@6c2b1a8 Token{a1b2c3 ActivityRecord{ff00 u0 com.example/.Main t12}}",
		// stack frames
		"#00 pc 00000000000943f8  /apex/com.android.runtime/lib64/bionic/libc.so (syscall+24)",
		"#01 pc 0000000013812345  /data/app/com.example.logtest-1/lib/x86_64/liblogtest.so (crash+12)",
		"at com.example.logtest.Triggers.run(Triggers.java:22)",
		"at java.lang.reflect.Method.invoke(Native Method)",
		"\tat com.example.Pay.charge(Pay.kt:4111)",
	} {
		negative(t, r, s)
	}
}

// Every line of the real API 29 captures keeps its header (time, uid, pid,
// tid, level, tag) and still parses after redacting the message, and only
// genuinely sensitive kinds are hit.
func TestRealCapturesStructureAndHits(t *testing.T) {
	r := newT(t, Options{})
	files, _ := filepath.Glob("../testdata/api29/*")
	if len(files) == 0 {
		testhome.SkipOrFail(t, "no fixtures under ../testdata/api29")
	}
	allowed := map[string]bool{"ipv4": true, "ipv6": true, "device_id": true, "phone": true, "domain": true}
	// every real host name present in the fixtures; anything else hit by the
	// domain rule is a false positive (package names, classes, files, props...)
	hosts := []string{"android.googlesource.com", "api.example.com", "connectivitycheck.gstatic.com",
		"epc.tmobile.com", "goo.gl", "mms.msg.eng.t-mobile.com", "play.googleapis.com",
		"push.example.com", "time.android.com", "www.google.com", "www.gstatic.com"}
	domainHits := 0
	reHit := regexp.MustCompile(`\[REDACTED:([a-z_0-9]+)#\d+\]`)
	lines := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range strings.Split(string(b), "\n") {
			p := logcat.ParseLine(ln, time.UTC, 2026)
			var out string
			if p.OK {
				// redact only the message, exactly like the report does
				i := strings.Index(ln, ": ")
				hdr := ln[:i+2]
				out = hdr + r.Text(ln[i+2:])
				q := logcat.ParseLine(out, time.UTC, 2026)
				if !q.OK || q.TimeNs != p.TimeNs || q.PID != p.PID || q.TID != p.TID || q.UID != p.UID || q.Level != p.Level || q.Tag != p.Tag {
					t.Fatalf("%s: header changed:\n%s\n%s", filepath.Base(f), ln, out)
				}
				lines++
			} else {
				out = r.Text(ln)
			}
			for _, m := range reHit.FindAllStringSubmatch(out, -1) {
				if !allowed[m[1]] {
					t.Errorf("%s: unexpected %s hit: %s", filepath.Base(f), m[1], out)
				}
				if m[1] == "domain" {
					domainHits++
					stripped := ln
					for _, h := range hosts {
						stripped = strings.ReplaceAll(stripped, h, "HOST")
					}
					if o := r.Text(stripped); strings.Contains(o, "[REDACTED:domain#") {
						t.Errorf("%s: domain hit on a non-host: %s", filepath.Base(f), o)
					}
				}
				if m[1] == "phone" && !strings.Contains(out, "mNumber=") {
					t.Errorf("%s: phone hit outside mNumber=: %s", filepath.Base(f), out)
				}
			}
		}
	}
	if lines < 10000 {
		t.Fatalf("only %d log lines checked", lines)
	}
	if domainHits < 20 {
		t.Fatalf("only %d domain hits; URL/bare hosts in fixtures not redacted", domainHits)
	}
}
