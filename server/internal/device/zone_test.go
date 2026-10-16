package device

import (
	"strings"
	"testing"
	"time"
)

// `date +%z` is the source of truth; persist.sys.timezone is used only when it agrees.
func TestResolveZone(t *testing.T) {
	t.Parallel()
	at := time.Unix(1791572000, 0)
	l, label := ResolveZone("Asia/Kuching", "+0800", at)
	if l == nil || l.String() != "Asia/Kuching" || label != "Asia/Kuching" {
		t.Fatalf("consistent zone: %v %q", l, label)
	}
	l, label = ResolveZone("America/New_York", "+0800", at)
	if _, off := at.In(l).Zone(); off != 8*3600 || !strings.Contains(label, "disagrees") {
		t.Fatalf("mismatch must fall back to the offset: %v %q", l, label)
	}
	l, _ = ResolveZone("", "-0330", at)
	if _, off := at.In(l).Zone(); off != -(3*3600 + 1800) {
		t.Fatalf("offset only: %v", l)
	}
	if l, _ := ResolveZone("", "", at); l != nil {
		t.Fatal("nothing known must return nil")
	}
}
