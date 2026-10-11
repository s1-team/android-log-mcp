package procs

import (
	"fmt"
	"slices"
	"testing"
)

// pm list packages -U excerpt with the shapes of issue #41: a shared uid
// (gms + gsf), installed packages that extend each other at a '.'
// (providers.downloads / .ui), a shared uid whose device process matches
// none of its packages (android.process.acore), and com.foo next to com.foobar.
const ownerPackages = `package:com.google.android.gms uid:10115
package:com.google.android.gsf uid:10115
package:com.android.providers.downloads uid:10056
package:com.android.providers.downloads.ui uid:10056
package:com.android.providers.contacts uid:10054
package:com.android.contacts uid:10054
package:com.android.calllogbackup uid:10054
package:com.foo uid:10201
package:com.foobar uid:10202
package:android uid:1000
package:com.android.settings uid:1000
`

var ownerCases = []struct {
	name string
	uid  int
	want string
}{
	// dotted device processes on the shared gms/gsf uid
	{"com.google.android.gms.persistent", 10115, "com.google.android.gms"},
	{"com.google.android.gms.unstable", 10115, "com.google.android.gms"},
	{"com.google.android.gms", 10115, "com.google.android.gms"},
	{"com.google.android.gms.ui:sub", 10115, "com.google.android.gms"},
	{"com.google.android.gsf.login", 10115, "com.google.android.gsf"},
	// the same names when the uid is not in the list (or not known)
	{"com.google.android.gms.persistent", 10999, "com.google.android.gms"},
	{"com.google.android.gms.persistent", -1, "com.google.android.gms"},
	{"com.google.android.gms.persistent", 1010115, "com.google.android.gms"}, // user 10
	// an installed package that extends another one is its own owner
	{"com.android.providers.downloads.ui", 10056, "com.android.providers.downloads.ui"},
	{"com.android.providers.downloads.ui", 10999, "com.android.providers.downloads.ui"},
	{"com.android.providers.downloads", 10056, "com.android.providers.downloads"},
	// negatives: cut only at '.', only towards installed packages
	{"com.foobar.sync", 10999, "com.foobar"},
	{"com.foobar:sync", 10999, "com.foobar"},
	{"com.foobar", 10202, "com.foobar"},
	{"com.google.x", 10999, "com.google.x"},     // not com.google (not installed)
	{"com.google.x:svc", 10999, "com.google.x"}, // the ':' fallback as before
	// shared uid, no name match: the uid's alphabetically first package (Q1)
	{"android.process.acore", 10054, "com.android.calllogbackup"},
	// unchanged fallbacks
	{"com.example.iso", 99001, "com.example.iso"}, // isolated
	{"system_server", 1000, ""},
	{"com.android.settings", 1000, "com.android.settings"},
	{"com.android.settings.extra", 1000, ""}, // system uid: exact name only
	{"/system/bin/sh", 10115, ""},
}

func TestOwner(t *testing.T) {
	uidPkgs := ParsePackagesU(ownerPackages)
	installed := map[string]bool{}
	for _, l := range uidPkgs {
		for _, p := range l {
			installed[p] = true
		}
	}
	for _, c := range ownerCases {
		if got := Owner(c.name, c.uid, uidPkgs, installed); got != c.want {
			t.Errorf("Owner(%q, %d) = %q, want %q", c.name, c.uid, got, c.want)
		}
	}
	// the shared-uid pick does not depend on the order of the list
	rev := map[int][]string{}
	for u, l := range uidPkgs {
		r := slices.Clone(l)
		slices.Reverse(r)
		rev[u] = r
	}
	if got := Owner("android.process.acore", 10054, rev, installed); got != "com.android.calllogbackup" {
		t.Errorf("reversed list: %q", got)
	}
	// helper binaries resolve by uid only
	if got := Owner("", 10202, uidPkgs, installed); got != "com.foobar" {
		t.Errorf("helper: %q", got)
	}
}

// The tracker gives the same answer whether the package list arrives
// before or after the process table (issue #41: entries created first kept
// their fallback name forever), and after a Restore.
func TestTrackerAttributionBothOrders(t *testing.T) {
	var ps []PSEntry
	for i, c := range ownerCases {
		if c.uid >= 0 {
			ps = append(ps, PSEntry{PID: 1000 + i, UID: c.uid, Name: c.name})
		}
	}
	check := func(order string, tr *Tracker) {
		t.Helper()
		got := map[int]string{}
		for _, l := range tr.byPID {
			for _, p := range l {
				got[p.PID] = p.Package
			}
		}
		for _, e := range ps {
			want := ownerCases[e.PID-1000].want
			if got[e.PID] != want {
				t.Errorf("%s: %s uid %d → %q, want %q", order, e.Name, e.UID, got[e.PID], want)
			}
		}
	}
	pk := ParsePackagesU(ownerPackages)

	a := New()
	a.Reset("b1")
	a.SetPackages(pk)
	a.Seed(ps, 5e9)
	check("packages, then ps", a)

	b := New()
	b.Reset("b1")
	b.Seed(ps, 5e9)
	var changed []Proc
	b.OnChange = func(p Proc) { changed = append(changed, p) }
	b.SetPackages(pk)
	check("ps, then packages", b)
	persisted := false
	for _, p := range changed {
		if p.Name == "com.google.android.gms.persistent" && p.Package == "com.google.android.gms" {
			persisted = true
		}
	}
	if !persisted {
		t.Errorf("re-attributed entry not passed to OnChange: %+v", changed)
	}

	// entries persisted with the fallback (procs.jsonl of 0.5.x), restored
	// into a tracker that already has the list
	var saved []Proc
	for _, e := range ps {
		saved = append(saved, Proc{Boot: "b1", PID: e.PID, UID: e.UID, Name: e.Name, Package: e.Name, Source: "ps", SeenNs: 5e9})
	}
	c := New()
	c.Reset("b1")
	c.SetPackages(pk)
	c.Restore(saved)
	check("restore after packages", c)
	d := New()
	d.Reset("b1")
	d.Restore(saved)
	d.SetPackages(pk)
	check("restore, then packages", d)
}

// A device process whose application uid is not in the package list asks
// for the list again (OnUnknownUID); known uids and an unloaded list do not.
func TestTrackerUnknownUID(t *testing.T) {
	tr := New()
	tr.Reset("b1")
	var asked []int
	tr.OnUnknownUID = func(uid int) { asked = append(asked, uid) }
	tr.Seed([]PSEntry{{PID: 10, UID: 10999, Name: "com.new.app"}}, 1e9)
	if len(asked) != 0 {
		t.Fatalf("asked before the list was loaded: %v", asked)
	}
	tr.SetPackages(ParsePackagesU(ownerPackages))
	tr.Seed([]PSEntry{{PID: 11, UID: 10115, Name: "com.google.android.gms.persistent"}, {PID: 12, UID: 99001, Name: "com.x.iso"}}, 2e9)
	if len(asked) != 0 {
		t.Fatalf("asked for a known or isolated uid: %v", asked)
	}
	tr.Seed([]PSEntry{{PID: 13, UID: 1010998, Name: "com.newer.app"}}, 3e9)
	if fmt.Sprint(asked) != "[10998]" {
		t.Fatalf("asked = %v, want [10998]", asked)
	}
}
