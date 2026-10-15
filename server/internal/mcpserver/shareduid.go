package mcpserver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/s1crab/android-log-mcp/server/internal/procs"
)

// sharedUID is the package= scope of a package that shares its uid with
// other packages (issue #44): device processes of the uid whose names match
// none of its packages (android.process.acore for com.android.providers.contacts,
// com.android.contacts …) can run components of any of them, so their lines
// and crashes are attributed to no package and package= includes them for
// every package of the uid, with a header note naming the packages.
type sharedUID struct {
	pkg   string
	appID int
	pkgs  []string
	names map[string]bool // shared-uid device process names seen (Procs)
}

// newSharedUID returns nil when pkg is not on a shared uid; ps (the stored
// process lifetimes) is loaded only for a shared uid.
func newSharedUID(pkg string, uidPkgs map[int][]string, ps func() []procs.Proc) *sharedUID {
	a, pkgs, ok := procs.SharedUIDOf(uidPkgs, pkg)
	if !ok {
		return nil
	}
	s := &sharedUID{pkg: pkg, appID: a, pkgs: pkgs, names: map[string]bool{}}
	for _, p := range ps() {
		// dotted names only: helper binaries (sh, crash_dump64) are not
		// app processes
		if procs.AppID(p.UID) == a && strings.Contains(p.Name, ".") && procs.MatchesNone(p.Name, pkgs) {
			s.names[p.Name] = true
		}
	}
	return s
}

// line reports whether a line with this uid and device process belongs to
// the shared scope: the uid is known and the process matches none of the
// uid's packages.
func (s *sharedUID) line(uid int, process string) bool {
	return s != nil && uid >= 0 && procs.AppID(uid) == s.appID && procs.MatchesNone(process, s.pkgs)
}

// process reports whether a device process name (no uid known: stored
// crashes) is a shared-uid process of the scope.
func (s *sharedUID) process(name string) bool { return s != nil && s.names[name] }

func (s *sharedUID) note() string {
	var ns []string
	for n := range s.names {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	what := "processes of the uid that match none of its packages are"
	if len(ns) == 1 {
		what = ns[0] + " (shared-uid process) is"
	} else if len(ns) > 1 {
		what = strings.Join(ns, ", ") + " (shared-uid processes) are"
	}
	return fmt.Sprintf("package %s shares uid %d with %s: %s included and may belong to any of these packages.",
		s.pkg, s.appID, strings.Join(others(s.pkgs, s.pkg), ", "), what)
}

func others(l []string, x string) []string {
	var o []string
	for _, s := range l {
		if s != x {
			o = append(o, s)
		}
	}
	return o
}

func crashPackage(pkg, process string) string { return procs.CrashPackage(pkg, process) }

// noteLine is the shared-uid note of a search response: the count and how
// to list the packages by default (the 24-package list is pure token cost
// for a single package query), the full list with verbose.
func (s *sharedUID) noteLine(verbose bool) string {
	oth := others(s.pkgs, s.pkg)
	kv := []any{"code", "shares_uid", "package", s.pkg, "uid", s.appID, "shares_uid", len(oth)}
	if !verbose {
		return hdr("note", kv...) + nextClause("verbose=true")
	}
	var ns []string
	for n := range s.names {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	kv = append(kv, "with", strings.Join(oth, ","), "shared_processes", strings.Join(ns, ","))
	return hdr("note", kv...)
}
