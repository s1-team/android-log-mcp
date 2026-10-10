package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Log file bounds: service.log plus one rotated service.log.1.
const (
	LogMaxBytes = 1 << 20
)

// LogDir is the service's log directory under the data root.
func LogDir(root string) string { return filepath.Join(root, "log") }

// LogFile is the service log: <root>/log/service.log, rotated to
// service.log.1 at LogMaxBytes (so at most 2 × LogMaxBytes on disk, counted
// in the data root's total size cap). SetRoot moves it with set-root.
type LogFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	Max  int64
}

// OpenLog opens (appends to) the log under root.
func OpenLog(root string) (*LogFile, error) {
	l := &LogFile{Max: LogMaxBytes}
	return l, l.SetRoot(root)
}

// Path is the current log file.
func (l *LogFile) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// SetRoot switches the log to root's log directory.
func (l *LogFile) SetRoot(root string) error {
	dir := LogDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, "service.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	l.mu.Lock()
	old := l.f
	l.f, l.path = f, p
	l.size = 0
	if fi != nil {
		l.size = fi.Size()
	}
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (l *LogFile) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(b), nil
	}
	if l.size+int64(len(b)) > l.Max && l.size > 0 {
		l.f.Close()
		os.Rename(l.path, l.path+".1") // replaces the previous .1
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			l.f = nil
			return 0, err
		}
		l.f, l.size = f, 0
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *LogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// CrashFile opens <root>/log/crash.log for the Go runtime's fatal error
// output (debug.SetCrashOutput); the previous one is kept as crash.log.1.
func CrashFile(root string) (*os.File, error) {
	dir := LogDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "crash.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
		os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

var _ io.Writer = (*LogFile)(nil)

func describeErr(prefix string, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", prefix, err)
}
