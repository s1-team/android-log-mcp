package device

import (
	"os"
	"path/filepath"
	"strings"
)

// Killing an adb child together with its process tree (issue #24): a
// cancelled adb client is killed with the descendants it started, so no
// grandchild keeps its output pipe open or touches files after the
// recorder stopped. The adb server is never killed: an adb client can
// start it (`adb fork-server server`), so it may be a descendant (on unix
// even in our process group). Any process named adb below the root is
// taken for a server and skipped with its subtree (an adb client never
// starts another adb client). That is why this walks the tree instead of
// killing a process group or a Windows job object.

// procEntry is one process of the host process table.
type procEntry struct {
	pid, ppid int
	name      string // executable base name
}

// isADBName reports whether an executable name is adb (the server).
func isADBName(name string) bool {
	n := strings.ToLower(filepath.Base(name))
	return strings.TrimSuffix(n, ".exe") == "adb"
}

// descendants of root in procs, parents before children, without adb
// processes and their subtrees; child(parent, pid) confirms a parent link
// (Windows: pids are reused and parent ids are never updated).
func descendants(root int, procs []procEntry, child func(parent, pid int) bool) []int {
	kids := map[int][]procEntry{}
	for _, p := range procs {
		if p.pid != p.ppid {
			kids[p.ppid] = append(kids[p.ppid], p)
		}
	}
	var out []int
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, k := range kids[parent] {
			if seen[k.pid] || isADBName(k.name) || (child != nil && !child(parent, k.pid)) {
				continue
			}
			seen[k.pid] = true
			out = append(out, k.pid)
			queue = append(queue, k.pid)
		}
	}
	return out
}

// killTree kills p and its descendants (except adb servers). The tree is
// read before p dies (its orphans would be re-parented). Errors listing
// the tree only leave the descendants alive; p is killed regardless.
func killTree(p *os.Process) error {
	var desc []int
	if procs, err := listProcs(); err == nil {
		desc = descendants(p.Pid, procs, isChild)
	}
	err := p.Kill()
	for _, pid := range desc {
		killPid(pid)
	}
	return err
}
