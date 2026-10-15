package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/ipc"
)

// What this file does (issue #83): tell a service that answers from one that
// holds the lock but does not, find out whether the holder is really this
// tool's service, and stop it (service stop --force, and the automatic
// replacement a client makes when a service is silent for too long).

// probeState is the result of probing the service socket with a hello.
type probeState int

const (
	probeAnswers probeState = iota // hello answered
	probeAbsent                    // no listener (dial failed or closed at once)
	probeSilent                    // connected, but no hello reply in time
)

// probe dials the socket and says hello; a bare connect proves nothing (a
// socket in the kernel backlog connects even when the service never
// accepts).
func probe(ctx context.Context, ctrl Control, dialWait, helloWait time.Duration) probeState {
	c, err := ipc.Dial(ctrl.Socket(), dialWait)
	if err != nil {
		return probeAbsent
	}
	defer c.Close()
	hctx, cancel := context.WithTimeout(ctx, helloWait)
	defer cancel()
	var r HelloReply
	err = c.Do(hctx, "hello", Hello{Proto: Proto, Version: "probe", PID: os.Getpid(), Role: "cli"}, &r)
	var re *ipc.RemoteError
	switch {
	case err == nil, errors.As(err, &re):
		return probeAnswers
	case hctx.Err() != nil:
		return probeSilent
	default:
		return probeAbsent
	}
}

// Probe reports whether the service of ctrl answers a hello.
func Probe(ctx context.Context, ctrl Control) (answers bool) {
	return probe(ctx, ctrl, 2*time.Second, 3*time.Second) == probeAnswers
}

// Diagnosis describes who holds the service lock.
type Diagnosis struct {
	Held      bool // the lock is held
	HaveState bool // run/service.json was readable
	PID       int
	Alive     bool // PID exists
	Ours      bool // PID is the service recorded in service.json (image matches)
	Strong    bool // ... and its start time matches too (needed for an automatic kill)
	Embedded  bool // the service runs inside an MCP process
	Stopping  bool // service.log says it is shutting down, and the log is fresh
	Exe       string
	Version   string
	Why       string // why not Ours
	State     *State
}

// Diagnose looks at the lock, service.json and the service log.
func Diagnose(ctrl Control) Diagnosis {
	d := Diagnosis{Held: Running(ctrl)}
	st := ctrl.ReadState()
	if st == nil {
		d.Why = "no run/service.json"
		return d
	}
	d.State, d.HaveState, d.PID, d.Embedded, d.Exe, d.Version = st, true, st.PID, st.Embedded, st.Exe, st.Version
	d.Alive, d.Ours, d.Strong, d.Why = identify(st)
	d.Stopping = logSaysStopping(st.Root)
	return d
}

// identify checks that st.PID is the service that wrote st and not a process
// that reused the pid: same image and, when recorded, the same start time.
func identify(st *State) (alive, ours, strong bool, why string) {
	if st.PID <= 0 {
		return false, false, false, "no pid"
	}
	start, exe, ok := inspectProc(st.PID)
	if !ok {
		return false, false, false, "that process does not exist"
	}
	if st.Exe == "" || exe == "" || !sameExe(st.Exe, exe) {
		return true, false, false, fmt.Sprintf("its image is %q, not %q", exe, st.Exe)
	}
	if st.StartTime != 0 && start != st.StartTime {
		return true, false, false, "its start time differs from the service's (the pid was reused)"
	}
	return true, true, st.StartTime != 0, ""
}

func sameExe(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b)) {
		return true
	}
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}

// logSaysStopping: the last line of service.log is "service stopping …" and
// the file was written in the last 15 s.
func logSaysStopping(root string) bool {
	if root == "" {
		return false
	}
	p := filepath.Join(LogDir(root), "service.log")
	fi, err := os.Stat(p)
	if err != nil || time.Since(fi.ModTime()) > 15*time.Second {
		return false
	}
	return strings.Contains(lastLine(p), "service stopping")
}

func lastLine(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	n := int64(4096)
	if fi.Size() < n {
		n = fi.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, fi.Size()-n); err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\r\n"), "\n")
	return lines[len(lines)-1]
}

// Text is the one-line description used in errors and the offline report.
func (d Diagnosis) Text(ctrl Control) string {
	var b strings.Builder
	switch {
	case !d.Held:
		b.WriteString("lock free (no service holds " + ctrl.LockPath() + ")")
	case !d.HaveState:
		b.WriteString("lock held, holder unknown (" + d.Why + ")")
	case !d.Alive:
		fmt.Fprintf(&b, "lock held, service.json names pid %d, which does not exist (a child process may still hold the lock)", d.PID)
	case d.Ours && d.Embedded:
		fmt.Fprintf(&b, "lock held by pid %d, an in-process service inside an MCP process (version %s)", d.PID, d.Version)
	case d.Ours:
		fmt.Fprintf(&b, "lock held by pid %d, this tool's service (version %s)", d.PID, d.Version)
	default:
		fmt.Fprintf(&b, "lock held; service.json names pid %d, but %s", d.PID, d.Why)
	}
	if d.Stopping {
		b.WriteString("; its log says it is stopping")
	}
	if _, err := os.Lstat(ctrl.Socket()); err != nil {
		b.WriteString("; socket file missing")
	}
	return b.String()
}

// StopResult says how StopHolder ended the holder.
type StopResult struct {
	Killed bool // the graceful stop did not work and the process was killed
}

// StopOptions tunes StopHolder.
type StopOptions struct {
	// Auto: a client replacing a silent service; requires a strong identity
	// (image and start time) and refuses an in-process service. Without it
	// (service stop --force) an image match is enough when service.json has
	// no start time (written by an older version).
	Auto     bool
	TermWait time.Duration // wait for a graceful stop (default 10 s)
	KillWait time.Duration // wait for the lock after the kill (default 5 s)
	// Killing is called just before the process is killed (log the data loss).
	Killing func(pid int)
}

// ErrNotOurs: the holder is not recognised as this tool's service, so it is
// not signalled.
var ErrNotOurs = errors.New("the lock holder is not recognised as this service")

// StopHolder stops the service process named in service.json while it holds
// the lock: graceful first (SIGTERM; on Windows the stop op, which only works
// while the service answers), then, after TermWait, SIGKILL / TerminateProcess,
// then it removes the stale socket and service.json. The pid is verified
// again right before the kill.
func StopHolder(ctrl Control, d Diagnosis, o StopOptions) (StopResult, error) {
	if o.TermWait <= 0 {
		o.TermWait = 10 * time.Second
	}
	if o.KillWait <= 0 {
		o.KillWait = 5 * time.Second
	}
	if !d.HaveState || !d.Alive {
		return StopResult{}, fmt.Errorf("%w: %s", ErrNotOurs, d.Why)
	}
	if !d.Ours || (o.Auto && (!d.Strong || d.Embedded)) {
		why := d.Why
		switch {
		case d.Ours && d.Embedded:
			why = "it runs inside an MCP process, which this tool does not kill"
		case d.Ours:
			why = "service.json has no start time to rule out a reused pid"
		}
		return StopResult{}, fmt.Errorf("%w: %s", ErrNotOurs, why)
	}
	pid := d.PID
	if hasGracefulSignal {
		terminateProc(pid)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		if c, err := Dial(ctx, ctrl, "stop", "cli"); err == nil {
			c.Do(ctx, "stop", nil, nil)
			c.Close()
		}
		cancel()
	}
	if WaitStopped(ctrl, o.TermWait) {
		cleanupStale(ctrl, pid)
		return StopResult{}, nil
	}
	// the pid may have been reused while we waited
	st := ctrl.ReadState()
	if st == nil || st.PID != pid {
		return StopResult{}, fmt.Errorf("service.json changed while waiting; not killing pid %d", pid)
	}
	if alive, ours, strong, why := identify(st); !alive {
		cleanupStale(ctrl, pid)
		return StopResult{}, nil
	} else if !ours || (o.Auto && !strong) {
		return StopResult{}, fmt.Errorf("%w: %s", ErrNotOurs, why)
	}
	if o.Killing != nil {
		o.Killing(pid)
	}
	if err := killProc(pid); err != nil {
		return StopResult{}, fmt.Errorf("killing pid %d: %v", pid, err)
	}
	if !WaitStopped(ctrl, o.KillWait) {
		return StopResult{Killed: true}, fmt.Errorf("pid %d was killed but the lock is still held (another process may have inherited it)", pid)
	}
	cleanupStale(ctrl, pid)
	return StopResult{Killed: true}, nil
}

// cleanupStale removes the socket and service.json a stopped service left.
func cleanupStale(ctrl Control, pid int) {
	if st := ctrl.ReadState(); st != nil && st.PID == pid {
		os.Remove(ctrl.StatePath())
	}
	os.Remove(ctrl.Socket())
}

// replaceLimit: at most 3 forced replacements per minute per user, counted in
// run/replace.json, so a service that hangs right after every start is not
// killed in a loop. Call with spawn.lock held.
func allowReplace(ctrl Control, now time.Time) bool {
	p := filepath.Join(ctrl.Run(), "replace.json")
	var ts []int64
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &ts)
	}
	kept := ts[:0]
	for _, t := range ts {
		if now.Sub(time.Unix(0, t)) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 3 {
		return false
	}
	kept = append(kept, now.UnixNano())
	b, _ := json.Marshal(kept)
	writeFileAtomic(p, b, 0o600)
	return true
}
