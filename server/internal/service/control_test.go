package service

// Issue #108: the socket path of a Control is run/service.sock, or
// /tmp/alm-<uid>/<hash>.sock when that is longer than 100 bytes (never a
// file in the cache dir). Tests go through DefaultControl().Socket().

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// cacheWithPrimaryLen points the user cache dir at a directory such that
// run/service.sock is exactly n bytes long and returns the control.
func cacheWithPrimaryLen(t *testing.T, n int) Control {
	t.Helper()
	k := 10
	for i := 0; i < 3; i++ {
		d := "/tmp/" + strings.Repeat("a", k)
		t.Setenv("HOME", d)
		t.Setenv("XDG_CACHE_HOME", d)
		got := len(filepath.Join(DefaultControl().Run(), "service.sock"))
		if got == n {
			return DefaultControl()
		}
		k += n - got
	}
	t.Fatalf("cannot build a cache dir for primary length %d", n)
	return Control{}
}

func TestControlSocketLongPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows uses the temp dir")
	}
	re := regexp.MustCompile(fmt.Sprintf(`^/tmp/alm-%d/[0-9a-f]{16}\.sock$`, os.Getuid()))
	seen := map[string]bool{}
	for _, n := range []int{73, 99, 100, 101, 102, 108, 140} {
		c := cacheWithPrimaryLen(t, n)
		got := c.Socket()
		if n <= 100 {
			if want := filepath.Join(c.Run(), "service.sock"); got != want {
				t.Fatalf("primary %d bytes: %s, want %s", n, got, want)
			}
			continue
		}
		if !re.MatchString(got) || len(got) > 100 {
			t.Fatalf("primary %d bytes: %s, want /tmp/alm-<uid>/<16 hex>.sock", n, got)
		}
		if c.Socket() != got {
			t.Fatalf("not stable: %s", got)
		}
		if seen[got] {
			t.Fatalf("same socket for different cache dirs: %s", got)
		}
		seen[got] = true
	}
}
