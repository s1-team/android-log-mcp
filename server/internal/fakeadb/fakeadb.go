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
	return (b == "adb" || b == helperName) && os.Getenv(EnvDir) != ""
}

// helperName is the grandchild a hanging shell command starts (SetHang):
// not named adb, so it is not taken for an adb server.
const helperName = "adbhang"

// Install copies the running test binary to dir/bin/adb[.exe] and returns
// its path (point ANDROID_LOG_MCP_ADB at it, and set EnvDir=dir).
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
	return dst, os.WriteFile(dst, b, 0o755)
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

func writeAtomic(p, s string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
	e := Env{Dir: os.Getenv(EnvDir)}
	appendLine(filepath.Join(e.Dir, "pids"), strconv.Itoa(os.Getpid()))
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
		fmt.Print("List of devices attached\n" + readDevices(e) + "\n")
		return 0
	case "track-devices":
		appendLine(filepath.Join(e.Dir, "track"), strconv.Itoa(os.Getpid()))
		last := "\x00"
		for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			cur := readDevices(e)
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

func readDevices(e Env) string {
	b, _ := os.ReadFile(filepath.Join(e.Dir, "devices"))
	return string(b)
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
	case strings.HasPrefix(cmd, "logcat -d -t 1"):
		if l := bufferLines(e, serial); len(l) > 0 {
			fmt.Println(l[len(l)-1])
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
		for ; sent < len(lines); sent++ {
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
		if pid, _ := strconv.Atoi(l); pid > 0 && pid != os.Getpid() && Alive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// Hanging reports whether a hang grandchild was started.
func (e Env) Hanging() bool {
	_, err := os.Stat(filepath.Join(e.Dir, "hang_started"))
	return err == nil
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
		cmd := procattr.Hide(exec.Command(helper))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // holds our output pipes
		if cmd.Start() == nil {
			appendLine(filepath.Join(e.Dir, "hang_started"), strconv.Itoa(cmd.Process.Pid))
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
