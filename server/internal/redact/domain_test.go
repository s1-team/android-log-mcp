package redact

import (
	"regexp"
	"strings"
	"testing"
)

func TestDomainInURL(t *testing.T) {
	r := newT(t, Options{})
	cases := []struct{ in, want string }{
		{"GET https://api.foo.com/v1/login", "GET https://[REDACTED:domain#1]/v1/login"},
		{"--> POST http://api.foo.com:8443/v2/x?page=2 (12-byte body)", "--> POST http://[REDACTED:domain#1]:8443/v2/x?page=2 (12-byte body)"},
		{"ws: wss://push.bar.io/socket", "ws: wss://[REDACTED:domain#2]/socket"},
		{"open ftp://files.internal.corp/pub", "open ftp://[REDACTED:domain#3]/pub"}, // URL hosts: any TLD
		{"probe http://connectivitycheck.gstatic.com/generate_204", "probe http://[REDACTED:domain#4]/generate_204"},
		{"redirect to HTTPS://API.FOO.COM/", "redirect to HTTPS://[REDACTED:domain#5]/"},
		{"Unable to resolve host \"cdn.foo.com\": No address", "Unable to resolve host \"[REDACTED:domain#6]\": No address"},
	}
	for _, c := range cases {
		if got := r.Text(c.in); got != c.want {
			t.Errorf("%q\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	// stable: same host → same placeholder, independent of case of other parts
	if a, b := r.Text("https://api.foo.com/a"), r.Text("api.foo.com timed out"); !strings.Contains(b, "[REDACTED:domain#1]") || !strings.Contains(a, "[REDACTED:domain#1]") {
		t.Errorf("not stable: %q %q", a, b)
	}
	// together with the password / url param rules
	out := positive(t, r, "password", "connect https://bob:pa55w0rd@db.foo.com:5432/x", "pa55w0rd", "db.foo.com")
	if !regexp.MustCompile(`^connect https://bob:\[REDACTED:password#\d+\]@\[REDACTED:domain#\d+\]:5432/x$`).MatchString(out) {
		t.Errorf("userinfo/port structure: %q", out)
	}
	out = positive(t, r, "credential", "GET https://api.foo.com/v1?page=2&access_token=abcDEF123456ghi&lang=en", "abcDEF123456ghi", "api.foo.com")
	if !strings.Contains(out, "https://[REDACTED:domain#1]/v1?page=2&") || !strings.HasSuffix(out, "&lang=en") {
		t.Errorf("url structure: %q", out)
	}
	// IP literals stay IP (ipv4 rule), not domain
	if out := r.Text("GET http://203.0.113.9:8080/x"); strings.Contains(out, "domain#") || !strings.Contains(out, "ipv4#") {
		t.Errorf("ip url: %q", out)
	}
	if out := r.Text("addr 10.0.2.15 dns 8.8.8.8"); strings.Contains(out, "domain#") {
		t.Errorf("ip as domain: %q", out)
	}
	// java "host/ip" form
	positive(t, r, "domain", "failed to connect to www.google.com/203.0.113.4 (port 443)", "www.google.com")
}

func TestDomainBare(t *testing.T) {
	r := newT(t, Options{})
	positive(t, r, "domain", "resolving api.foo.com", "api.foo.com")
	positive(t, r, "domain", "ntp server sub.example.cn timed out", "sub.example.cn")
	positive(t, r, "domain", "host=img.cdn.example.co.uk", "img.cdn.example.co.uk")
	positive(t, r, "domain", "apn: epc.tmobile.com, proxy:", "epc.tmobile.com")
	positive(t, r, "domain", "[time.android.com/216.239.35.4] failed", "time.android.com")
	out := positive(t, r, "domain", "connect example.com:443 ok", "example.com")
	if !strings.HasSuffix(out, ":443 ok") {
		t.Errorf("port lost: %q", out)
	}
	out = positive(t, r, "domain", "see www.foo.org.", "www.foo.org")
	if !strings.HasSuffix(out, "].") {
		t.Errorf("sentence dot: %q", out)
	}
}

func TestDomainEmailConsistent(t *testing.T) {
	r := newT(t, Options{})
	out := r.Text("user bob@example.com from example.com")
	if !regexp.MustCompile(`^user \[REDACTED:email#1\] from \[REDACTED:domain#1\]$`).MatchString(out) {
		t.Errorf("email/domain: %q", out)
	}
	out = r.Text("mailto:alice@mail.foo.com")
	if strings.Contains(out, "foo.com") || !strings.Contains(out, "email#") {
		t.Errorf("mailto: %q", out)
	}
}

func TestDomainKeepList(t *testing.T) {
	r := newT(t, Options{KeepDomains: []string{"example.com", "googleapis.com"}})
	negative(t, r, "GET https://api.example.com/v1/login")
	negative(t, r, "resolving play.googleapis.com")
	negative(t, r, "resolving example.com")
	positive(t, r, "domain", "resolving notexample.com", "notexample.com") // suffix on label boundary
	positive(t, r, "domain", "GET https://api.foo.com/", "api.foo.com")
}

func TestDomainNegatives(t *testing.T) {
	r := newT(t, Options{})
	for _, s := range []string{
		// java / kotlin package and class names, reverse-DNS
		"Start proc 4321:com.example.logtest/u0a146 for activity com.example.logtest/.MainActivity",
		"at android.app.ActivityThread.main(ActivityThread.java:7356)",
		"java.lang.RuntimeException: Unable to start activity",
		"Caused by: java.io.IOException: boom",
		"kotlin.KotlinNullPointerException",
		"\tat com.foo.Bar.baz(Bar.java:42)",
		"\tat com.foo.net.Client$1.run(Client.kt:17)",
		"\tat okhttp3.internal.connection.RealCall.execute(RealCall.kt:154)",
		"io.reactivex.exceptions.OnErrorNotImplementedException",
		"net.sqlcipher.database.SQLiteException",
		"org.chromium.net.UrlRequest failed",
		"com.google.android.gms.persistent process started",
		"Process com.android.phone (pid 1234) has died",
		"ActivityManager: Killing 4321:com.example.logtest/u0a146 (adj 900): remove task",
		"Displayed com.example.logtest/.MainActivity: +412ms",
		"dat=app://com.google.android.apps.nexuslauncher:10108?v=9",
		"content://com.example.provider/items/1",
		"file:///data/app/com.example-1/base.apk",
		"android.resource://com.example.app/drawable/icon",
		"intent:#Intent;component=com.example/.Main;end",
		// file names
		"loaded Foo.java", "dlopen libc.so failed", "/system/lib64/libc.so", "installing base.apk",
		"dex2oat base.odex", "inflate activity_main.xml", "parse config.json", "res/layout/main.xml",
		"read app.db", "load index.html", "split_config.arm64_v8a.apk", "boot.art mapped",
		"android.hardware.camera.provider@2.4-service", "libandroid_runtime.so",
		// versions, IPs
		"versionName=1.2.3 versionCode=42", "Chrome/83.0.4103.106", "release 10.0.2",
		// properties
		"ro.build.fingerprint=google/sdk_gphone/generic:10/QSR1/1:userdebug",
		"persist.sys.timezone=Asia/Shanghai", "sys.boot_completed=1", "debug.hwui.renderer skiavk",
		"ctl.start vendor.audio-hal", "init: Service 'vendor.drm-widevine-hal-1-2' (pid 512) exited",
		"vendor.qti.hardware.perf@2.0", "dalvik.vm.heapsize 512m",
		// intent actions
		"act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER]",
		"Broadcast android.net.conn.CONNECTIVITY_CHANGE", "com.google.android.c2dm.intent.RECEIVE",
		// file extensions / words that are also TLDs but left out of the bare-host list
		"build main.rs", "run setup.py", "exec init.sh", "read README.md", "libfoo.ar", "libjpeg.so.8",
		"window.top", "user.online", "data.store", "app.shop", "page.site", "google.cloud",
		// misc
		"e.g. this is a sentence", "i.e. nothing", "Mr.Smith", "file.Close()", "obj.toString()",
		"this.mHandler", "R.id.button", "System.out", "Log.d", "a.b.c", "Bitmap.Config.ARGB_8888",
		"window.Window$Callback", "Thread.run", "Math.max",
	} {
		negative(t, r, s)
	}
}
