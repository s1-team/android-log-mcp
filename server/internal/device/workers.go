package device

import (
	"context"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Background work of a device or the manager (issue #24): goroutines that
// run adb besides the recorder itself (side work, DropBox fetches, buffer
// measurements; device tracking and identity probes). Stopping a device
// or closing the manager cancels them and waits until they returned, so
// no adb child of ours outlives it and nothing writes into a closed store.

// bgStopWait bounds the wait for background work after cancelling it;
// what is still running then is logged and its adb processes are killed.
var bgStopWait = newDur(5 * time.Second)

// adbWaitDelay bounds how long a cancelled adb call waits for its output
// pipes to close (a grandchild holding them must not hang the caller).
var adbWaitDelay = newDur(2 * time.Second)

// workers tracks running background goroutines by name.
type workers struct {
	mu   sync.Mutex
	next uint64
	run  map[uint64]string
	idle chan struct{} // closed when none runs
}

// spawn runs f in a goroutine tracked as name.
func (w *workers) spawn(name string, f func()) {
	w.mu.Lock()
	if w.run == nil {
		w.run = map[uint64]string{}
	}
	if len(w.run) == 0 {
		w.idle = make(chan struct{})
	}
	w.next++
	id := w.next
	w.run[id] = name
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.run, id)
			if len(w.run) == 0 {
				close(w.idle)
			}
			w.mu.Unlock()
		}()
		f()
	}()
}

// wait waits up to timeout until no work runs; it returns the names of
// the work still running then (sorted; nil when all returned).
func (w *workers) wait(timeout time.Duration) []string {
	w.mu.Lock()
	if len(w.run) == 0 {
		w.mu.Unlock()
		return nil
	}
	idle := w.idle
	w.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return nil
	case <-t.C:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var names []string
	for _, n := range w.run {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// children are the running adb processes started under a context that
// carries the set (withChildren): the last resort after bgStopWait.
type children struct {
	mu  sync.Mutex
	set map[*exec.Cmd]string
}

type childrenKey struct{}

// withChildren makes adb calls under ctx register their processes in c.
func withChildren(ctx context.Context, c *children) context.Context {
	return context.WithValue(ctx, childrenKey{}, c)
}

// trackChild registers a started cmd with the set of ctx (if any); the
// returned func unregisters it (call it after cmd.Wait).
func trackChild(ctx context.Context, cmd *exec.Cmd) func() {
	c, _ := ctx.Value(childrenKey{}).(*children)
	if c == nil || cmd.Process == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.set == nil {
		c.set = map[*exec.Cmd]string{}
	}
	c.set[cmd] = strings.Join(cmd.Args[1:], " ")
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.set, cmd)
		c.mu.Unlock()
	}
}

// killAll kills every registered process with its tree (never an adb
// server) and returns their command lines.
func (c *children) killAll() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var killed []string
	for cmd, args := range c.set {
		killTree(cmd.Process)
		killed = append(killed, "adb "+args)
	}
	slices.Sort(killed)
	return killed
}

// stopBackground waits (bgStopWait) for background work that was already
// cancelled; work still running then is logged, its adb processes are
// killed with their trees, and it gets one more second to return.
func stopBackground(who string, w *workers, c *children) {
	t := bgStopWait.Get()
	names := w.wait(t)
	if len(names) == 0 {
		return
	}
	killed := c.killAll()
	log.Printf("%s: background work did not stop within %s: %s; killed its adb processes: %s",
		who, t, strings.Join(names, ", "), orNone(killed))
	if names := w.wait(time.Second); len(names) > 0 {
		log.Printf("%s: background work still running: %s", who, strings.Join(names, ", "))
	}
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, "; ")
}

// waitBackground waits for the background work of a stopped device.
func (d *Device) waitBackground() { stopBackground(d.Serial, &d.bg, &d.kids) }
