// Package fakeadb is a scriptable fake adb for tests (issue #11): a test
// binary that finds itself invoked as adb (see Is) runs Main. Its state is
// a directory (EnvDir): the device list, per-device properties, log buffer
// and clock, and a log of every invocation, so tests can assert that an
// idle service runs no adb at all.
//
// Layout of the state directory:
//
//	calls.log              one line per invocation ("<unix ns> <arguments>")
//	devices                "serial state" lines (track-devices / devices)
//	dev/<serial>/props     identity properties, key=value lines (with boot_id)
//	dev/<serial>/buffer    the device log buffer, `-v epoch` threadtime lines
//	dev/<serial>/date      device clock override (`date +%s.%N` output)
//	dev/<serial>/delay_ms  delay per line while logcat dumps the buffer
//	dev/<serial>/logcat    pids of streaming logcat processes, one per line
//	dev/<serial>/no_exec_out  `adb exec-out` fails (adbd without exec, API < 21)
//	dev/<serial>/crlf      `adb logcat` writes CRLF line ends (pty)
package fakeadb

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/procattr"
)

// EnvDir names the state directory.
const EnvDir = "ALM_FAKEADB_DIR"

// Is reports whether this process was started as the fake adb.
func Is() bool {
	b := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	return (b == "adb" || b == helperName) && stateDir() != ""
}

// markerName marks a bin directory made by Install: a fake adb started
// from it finds its state directory without EnvDir, so tests can run in
// parallel without changing the process environment (#26).
const markerName = ".fakeadb"

// stateDir is EnvDir, or the parent of the bin directory a fake adb was
// installed in ("" when neither applies).
func stateDir() string {
	if d := os.Getenv(EnvDir); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	bin := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(bin, markerName)); err != nil {
		return ""
	}
	return filepath.Dir(bin)
}

// helperName is the grandchild a hanging shell command starts (SetHang):
// not named adb, so it is not taken for an adb server.
const helperName = "adbhang"

// Install copies the running test binary to dir/bin/adb[.exe] and returns
// its path (point ANDROID_LOG_MCP_ADB or Manager.ADBPath at it; the fake
// finds dir from its own location, EnvDir=dir is optional).
func Install(dir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "bin", name)
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", markerName), nil, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		return "", err
	}
	return dst, waitExecutable(dst)
}

// isBusy reports ETXTBSY ("text file busy"): exec of a file that some
// process still has open for writing.
func isBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "text file busy")
}

// waitExecutable runs the fresh copy once (`adb version`, with its own
// scratch state directory) until the kernel accepts it. A parallel test
// that forks while WriteFile has the copy open lets the child inherit the
// write descriptor until its exec, and exec of the copy then fails with
// ETXTBSY: the test's first adb call failed with no output, and tests that
// wait for the fake to start timed out (TestKillTreeKillsGrandchild, #103).
// Once one exec succeeded no process can hold a write descriptor any more.
func waitExecutable(path string) error {
	scratch, err := os.MkdirTemp("", "fakeadb-probe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	end := time.Now().Add(10 * time.Second)
	for {
		cmd := procattr.Hide(exec.Command(path, "version"))
		cmd.Env = append(os.Environ(), EnvDir+"="+scratch)
		err := cmd.Run()
		if !isBusy(err) || time.Now().After(end) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Env manipulates a state directory.
type Env struct{ Dir string }

func (e Env) devDir(serial string) string { return filepath.Join(e.Dir, "dev", serial) }

// SetDevices writes the device list ("serial" → adb state).
func (e Env) SetDevices(states map[string]string) error {
	var b strings.Builder
	for s, st := range states {
		fmt.Fprintf(&b, "%s\t%s\n", s, st)
	}
	return writeAtomic(filepath.Join(e.Dir, "devices"), b.String())
}

// AddDevice creates a device (not yet in the device list).
func (e Env) AddDevice(serial, bootID string) error {
	d := e.devDir(serial)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	props := fmt.Sprintf("ro.serialno=%s\nro.boot.serialno=%s\nro.kernel.qemu=\nro.boot.qemu=\nro.boot.qemu.avd_name=\nro.product.manufacturer=Fake\nro.product.model=Fake Phone\nro.build.fingerprint=fake/fake:10\nandroid_id=\nboot_id=%s\n", serial, serial, bootID)
	if err := os.WriteFile(filepath.Join(d, "props"), []byte(props), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "buffer"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Line formats one buffer line (`-v epoch` threadtime with uid).
func Line(t time.Time, pid int, tag, msg string) string {
	return fmt.Sprintf("%10d.%06d %5d %5d %5d I %-8s: %s", t.Unix(), t.Nanosecond()/1000, 10146, pid, pid, tag, msg)
}

// Log appends lines to a device's buffer.
func (e Env) Log(serial string, lines ...string) error {
	f, err := os.OpenFile(filepath.Join(e.devDir(serial), "buffer"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// DropLines removes buffer lines containing sub, as logd prunes one
// buffer (main) while another (system) keeps its oldest lines.
func (e Env) DropLines(serial, sub string) error {
	var keep []string
	for _, l := range bufferLines(e, serial) {
		if !strings.Contains(l, sub) {
			keep = append(keep, l+"\n")
		}
	}
	return writeAtomic(filepath.Join(e.devDir(serial), "buffer"), strings.Join(keep, ""))
}

// SetDate fixes the device clock reported by `date` (zero: real time).
func (e Env) SetDate(serial string, t time.Time) error {
	p := filepath.Join(e.devDir(serial), "date")
	if t.IsZero() {
		os.Remove(p)
		return nil
	}
	return writeAtomic(p, fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()))
}

// SetDelay slows logcat's buffer dump down to d per line.
func (e Env) SetDelay(serial string, d time.Duration) error {
	return writeAtomic(filepath.Join(e.devDir(serial), "delay_ms"), strconv.Itoa(int(d/time.Millisecond)))
}

// SetIdentDelay slows the identity probe (a slow device right after it
// connected) down by d.
func (e Env) SetIdentDelay(serial string, d time.Duration) error {
	return writeAtomic(filepath.Join(e.devDir(serial), "ident_delay_ms"), strconv.Itoa(int(d/time.Millisecond)))
}

// Call is one invocation: when it started and its arguments.
type Call struct {
	At   time.Time
	Args string
}

// CallLog returns every invocation so far, oldest first.
func (e Env) CallLog() []Call {
	b, _ := os.ReadFile(filepath.Join(e.Dir, "calls.log"))
	var out []Call
	for _, l := range strings.Split(string(b), "\n") {
		ts, args, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		ns, _ := strconv.ParseInt(ts, 10, 64)
		out = append(out, Call{At: time.Unix(0, ns), Args: args})
	}
	return out
}

// Calls returns every invocation's arguments so far.
func (e Env) Calls() []string {
	var out []string
	for _, c := range e.CallLog() {
		out = append(out, c.Args)
	}
	return out
}

// SetProp sets a device property (e.g. ro.build.version.sdk).
func (e Env) SetProp(serial, key, value string) error {
	p := props(e, serial)
	p[key] = value
	var b strings.Builder
	for k, v := range p {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return writeAtomic(filepath.Join(e.devDir(serial), "props"), b.String())
}

// SetNoExecOut makes `adb exec-out` fail like adbd without exec support
// (API < 21), and `adb logcat` write CRLF line ends (a pty).
func (e Env) SetNoExecOut(serial string) error {
	if err := writeAtomic(filepath.Join(e.devDir(serial), "no_exec_out"), "1"); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(e.devDir(serial), "crlf"), "1")
}

// CallsMatching returns the invocations containing sub.
func (e Env) CallsMatching(sub string) []string {
	var out []string
	for _, c := range e.Calls() {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

// LogcatRunning reports whether a streaming logcat for serial is alive.
func (e Env) LogcatRunning(serial string) bool {
	b, _ := os.ReadFile(filepath.Join(e.devDir(serial), "logcat"))
	for _, l := range strings.Fields(string(b)) {
		if pid, _ := strconv.Atoi(l); pid > 0 && Alive(pid) {
			return true
		}
	}
	return false
}

// AnyChildRunning reports whether any streaming logcat or track-devices
// started by the fake is alive.
func (e Env) AnyChildRunning() bool {
	ents, _ := os.ReadDir(filepath.Join(e.Dir, "dev"))
	for _, d := range ents {
		if e.LogcatRunning(d.Name()) {
			return true
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.Dir, "track"))
	for _, l := range strings.Fields(string(b)) {
		if pid, _ := strconv.Atoi(l); pid > 0 && Alive(pid) {
			return true
		}
	}
	return false
}

// writeAtomic replaces p (tmp file + rename). On Windows the rename fails
// while another process (a fake adb reading the file) has p open; it is
// retried briefly (#256).
func writeAtomic(p, s string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o644); err != nil {
		return err
	}
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		err := os.Rename(tmp, p)
		if err == nil || time.Now().After(end) {
			return err
		}
	}
}

func appendLine(p, s string) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.WriteString(s + "\n")
	f.Close()
}

// Main runs the fake adb with args (os.Args[1:]); returns the exit code.
func Main(args []string) int {
	e := Env{Dir: stateDir()}
	appendLine(filepath.Join(e.Dir, "pids"), pidLine(os.Getpid()))
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == helperName {
		return helperMain(e)
	}
	appendLine(filepath.Join(e.Dir, "calls.log"), strconv.FormatInt(time.Now().UnixNano(), 10)+" "+strings.Join(args, " "))
	if len(args) == 0 {
		return 1
	}
	switch args[0] {
	case "version":
		fmt.Println("Android Debug Bridge version 1.0.41")
		return 0
	case "start-server", "kill-server":
		return 0
	case "devices":
		cur, _ := readDevices(e)
		fmt.Print("List of devices attached\n" + cur + "\n")
		return 0
	case "track-devices":
		appendLine(filepath.Join(e.Dir, "track"), strconv.Itoa(os.Getpid()))
		last := "\x00"
		for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			cur, _ := readDevices(e)
			if cur != last {
				if _, err := fmt.Fprintf(os.Stdout, "%04x%s", len(cur), cur); err != nil {
					return 1
				}
				last = cur
			}
			if _, err := os.Stdout.Write(nil); err != nil {
				return 1
			}
		}
		return 0
	case "-s":
		if len(args) < 3 {
			return 1
		}
		serial := args[1]
		if _, err := os.Stat(e.devDir(serial)); err != nil {
			fmt.Fprintf(os.Stderr, "error: device '%s' not found\n", serial)
			return 1
		}
		switch args[2] {
		case "shell":
			return shell(e, serial, strings.Join(args[3:], " "))
		case "logcat":
			return logcat(e, serial, args[3:], crlf(e, serial))
		case "exec-out":
			if _, err := os.Stat(filepath.Join(e.devDir(serial), "no_exec_out")); err == nil {
				fmt.Fprintln(os.Stderr, "error: closed")
				return 1
			}
			if len(args) > 3 && args[3] == "logcat" {
				return logcat(e, serial, args[4:], false)
			}
			return shell(e, serial, strings.Join(args[3:], " "))
		case "emu":
			fmt.Println("KO: unsupported")
			return 1
		}
	}
	return 0
}

// readDevices is the device list. On Windows a read racing SetDevices'
// rename fails transiently (the file is being replaced: access denied,
// sharing violation, not found); such a read is retried briefly instead of
// being taken as an empty list, which the manager sees as every device gone
// (#256). A read that keeps failing returns "" with the error: the state
// directory was removed, and an orphaned track-devices then reports the
// empty list to its dead parent and exits.
func readDevices(e Env) (string, error) {
	p := filepath.Join(e.Dir, "devices")
	for end := time.Now().Add(500 * time.Millisecond); ; time.Sleep(5 * time.Millisecond) {
		b, err := os.ReadFile(p)
		if err == nil {
			return string(b), nil
		}
		if time.Now().After(end) || (os.IsNotExist(err) && runtime.GOOS != "windows") {
			return "", err // rename is atomic for readers outside Windows: missing is final
		}
	}
}

func props(e Env, serial string) map[string]string {
	m := map[string]string{}
	b, _ := os.ReadFile(filepath.Join(e.devDir(serial), "props"))
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

func date(e Env, serial string) string {
	if b, err := os.ReadFile(filepath.Join(e.devDir(serial), "date")); err == nil {
		return strings.TrimSpace(string(b))
	}
	now := time.Now()
	return fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond())
}

func bufferLines(e Env, serial string) []string {
	b, _ := os.ReadFile(filepath.Join(e.devDir(serial), "buffer"))
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// micros parses "sec.frac" into microseconds (exact, unlike a float).
func micros(s string) int64 {
	sec, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	n, _ := strconv.ParseInt(sec, 10, 64)
	frac = (frac + "000000")[:6]
	u, _ := strconv.ParseInt(frac, 10, 64)
	return n*1e6 + u
}

func lineTime(l string) int64 {
	f := strings.Fields(l)
	if len(f) == 0 {
		return 0
	}
	return micros(f[0])
}

func shell(e Env, serial, cmd string) int {
	if b, err := os.ReadFile(filepath.Join(e.devDir(serial), "hang")); err == nil {
		for _, sub := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if sub != "" && strings.Contains(cmd, sub) {
				return hang(e)
			}
		}
	}
	p := props(e, serial)
	switch {
	case strings.Contains(cmd, "boot_id"): // identity and/or recorder probe
		if strings.Contains(cmd, "ro.serialno") {
			if b, err := os.ReadFile(filepath.Join(e.devDir(serial), "ident_delay_ms")); err == nil {
				ms, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			for _, k := range []string{"ro.serialno", "ro.boot.serialno", "ro.kernel.qemu", "ro.boot.qemu", "ro.boot.qemu.avd_name", "ro.product.manufacturer", "ro.build.fingerprint", "android_id"} {
				fmt.Printf("%s=%s\n", k, p[k])
			}
		}
		sdk := p["ro.build.version.sdk"]
		if sdk == "" {
			sdk = "29"
		}
		fmt.Printf("ro.product.model=%s\nro.build.version.release=10\nro.build.version.sdk=%s\npersist.sys.timezone=UTC\nzone=+0000\nboot_id=%s\nnow=%s\n", p["ro.product.model"], sdk, p["boot_id"], date(e, serial))
	case strings.HasPrefix(cmd, "logcat -S"):
		fmt.Printf("size/num main               Total\nTotal    1000/%d      1000/%d\nNow      1000/%d      1000/%d\n", len(bufferLines(e, serial)), len(bufferLines(e, serial)), len(bufferLines(e, serial)), len(bufferLines(e, serial)))
	case strings.HasPrefix(cmd, "date"):
		fmt.Println(date(e, serial))
	case strings.HasPrefix(cmd, "logcat -d -t "):
		n, _ := strconv.Atoi(strings.Fields(cmd)[3])
		l := bufferLines(e, serial)
		if len(l) > n {
			l = l[len(l)-n:]
		}
		for _, x := range l {
			fmt.Println(x)
		}
	case strings.HasPrefix(cmd, "logcat -d") && strings.Contains(cmd, "| head -n"):
		n := 3
		if i := strings.LastIndex(cmd, "head -n"); i >= 0 {
			n, _ = strconv.Atoi(strings.TrimSpace(cmd[i+len("head -n"):]))
		}
		l := bufferLines(e, serial)
		if len(l) > n {
			l = l[:n]
		}
		for _, x := range l {
			fmt.Println(x)
		}
	case strings.HasPrefix(cmd, "ps"):
		fmt.Println("PID UID NAME")
	case strings.HasPrefix(cmd, "pm list packages"):
		fmt.Println("package:com.example.fake uid:10146")
	}
	return 0
}

func crlf(e Env, serial string) bool {
	_, err := os.Stat(filepath.Join(e.devDir(serial), "crlf"))
	return err == nil
}

func logcat(e Env, serial string, args []string, crlf bool) int {
	eol := "\n"
	if crlf {
		eol = "\r\n" // `adb logcat` through a pty on old devices
	}
	var from int64
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-T" {
			from = micros(args[i+1])
		}
	}
	appendLine(filepath.Join(e.devDir(serial), "logcat"), strconv.Itoa(os.Getpid()))
	delay := time.Duration(0)
	if b, err := os.ReadFile(filepath.Join(e.devDir(serial), "delay_ms")); err == nil {
		ms, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		delay = time.Duration(ms) * time.Millisecond
	}
	w := bufio.NewWriter(os.Stdout)
	sent := 0
	for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); {
		lines := bufferLines(e, serial)
		skip := streamSkip(e, serial)
		for ; sent < len(lines); sent++ {
			if skipped(lines[sent], sent, skip) {
				continue // like Android ≤ 11 logd kicking a reader (SetStreamSkip)
			}
			// like logd: -T <time> picks the start, the first line at
			// or after it in buffer order; every line after it follows,
			// whatever its time (a clock set back while streaming)
			if from > 0 && lineTime(lines[sent]) < from {
				continue
			}
			from = 0
			if _, err := w.WriteString(lines[sent] + eol); err != nil {
				return 1
			}
			if delay > 0 {
				if w.Flush() != nil {
					return 1
				}
				time.Sleep(delay)
			}
		}
		if w.Flush() != nil {
			return 1
		}
		if _, err := os.Stat(e.devDir(serial)); err != nil {
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// SetStreamSkip makes the live logcat stream leave out buffer lines that
// contain any of subs ("#n": the n-th buffer line, from 0), as Android ≤ 11 logd does when it tells a reader to
// skip entries while pruning (issue #65); `logcat -d` still prints them.
func (e Env) SetStreamSkip(serial string, subs ...string) error {
	return writeAtomic(filepath.Join(e.devDir(serial), "stream_skip"), strings.Join(subs, "\n")+"\n")
}

func streamSkip(e Env, serial string) []string {
	b, err := os.ReadFile(filepath.Join(e.devDir(serial), "stream_skip"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func skipped(line string, i int, subs []string) bool {
	for _, s := range subs {
		if n, err := strconv.Atoi(strings.TrimPrefix(s, "#")); err == nil && strings.HasPrefix(s, "#") {
			if n == i {
				return true
			}
		} else if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// SetHang makes shell commands containing any of subs hang: the fake
// starts a grandchild (not named adb) that inherits its output pipes, and
// both sleep for a minute; the grandchild then writes HelperLateFile.
func (e Env) SetHang(serial string, subs ...string) error {
	return writeAtomic(filepath.Join(e.devDir(serial), "hang"), strings.Join(subs, "\n")+"\n")
}

// HelperLateFile is written by a hang grandchild that was not killed.
const HelperLateFile = "late_write"

// Running returns the pids of fake adb processes (and hang grandchildren)
// that are still alive.
func (e Env) Running() []int {
	b, _ := os.ReadFile(filepath.Join(e.Dir, "pids"))
	var out []int
	for _, l := range strings.Fields(string(b)) {
		pid, id, _ := strings.Cut(l, ":")
		if n, _ := strconv.Atoi(pid); n > 0 && n != os.Getpid() && Alive(n) && sameProcess(n, id) {
			out = append(out, n)
		}
	}
	return out
}

// RunningSettled is Running for an immediate assertion after the caller
// killed and waited for its children: a killed grandchild that is still
// exiting (Exiting: it already closed its files and never runs user code
// again) is re-parented and reaped by the reaper of orphans, not by the
// caller, so this waits up to max while every running process is exiting.
// A process that is not exiting is returned at once, so a real hang still
// fails without delay.
func (e Env) RunningSettled(max time.Duration) []int {
	end := time.Now().Add(max)
	for {
		pids := e.Running()
		if len(pids) == 0 || time.Now().After(end) || slices.ContainsFunc(pids, func(p int) bool { return !Exiting(p) }) {
			return pids
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// CallsTail is the last n lines of the invocation log, for failure messages.
func (e Env) CallsTail(n int) string {
	b, _ := os.ReadFile(filepath.Join(e.Dir, "calls.log"))
	l := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.Join(l[max(0, len(l)-n):], "\n")
}

// Hanging reports whether a hang grandchild was started.
func (e Env) Hanging() bool {
	_, err := os.Stat(filepath.Join(e.Dir, "hang_started"))
	return err == nil
}

// HangError is why no hang grandchild was started ("" if none failed).
func (e Env) HangError() string {
	b, _ := os.ReadFile(filepath.Join(e.Dir, "hang_error"))
	return strings.TrimSpace(string(b))
}

func hang(e Env) int {
	exe, err := os.Executable()
	if err == nil {
		helper := filepath.Join(filepath.Dir(exe), helperName+filepath.Ext(exe))
		if _, serr := os.Stat(helper); serr != nil {
			if os.Link(exe, helper) != nil {
				if b, rerr := os.ReadFile(exe); rerr == nil {
					os.WriteFile(helper+".tmp", b, 0o755)
					os.Rename(helper+".tmp", helper)
				}
			}
		}
		var serr error
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			cmd := procattr.Hide(exec.Command(helper))
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // holds our output pipes
			if serr = cmd.Start(); serr == nil {
				appendLine(filepath.Join(e.Dir, "hang_started"), strconv.Itoa(cmd.Process.Pid))
				break
			}
			if !isBusy(serr) || time.Now().After(end) { // helper copy still open for writing
				break
			}
		}
		if serr != nil { // a test waiting for Hanging can say why it never will
			appendLine(filepath.Join(e.Dir, "hang_error"), serr.Error())
		}
	}
	time.Sleep(time.Minute)
	return 0
}

func helperMain(e Env) int {
	time.Sleep(time.Minute)
	os.WriteFile(filepath.Join(e.Dir, HelperLateFile), []byte("late"), 0o644)
	return 0
}
