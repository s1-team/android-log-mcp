package service

import (
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"
)

// Levelled logging of the client side (#84). MCP hosts such as Cursor label
// everything an MCP process writes to stderr as [error], so only warnings and
// errors go to stderr; info goes to the service log file only. Every level
// goes to the file when it can be opened.
type clientLog struct {
	mu    sync.Mutex
	file  io.Writer
	tried bool
}

func (l *clientLog) write(level string, toStderr bool, open func() io.Writer, format string, a ...any) {
	line := time.Now().Format("2006/01/02 15:04:05 ") + level + fmt.Sprintf(format, a...) + "\n"
	l.mu.Lock()
	if !l.tried {
		l.tried = true
		if open != nil {
			l.file = open()
		}
	}
	f := l.file
	l.mu.Unlock()
	if f != nil {
		io.WriteString(f, line)
	}
	if toStderr {
		io.WriteString(os.Stderr, line)
	}
}

// setFile replaces the file (the in-process service's own log file).
func (l *clientLog) setFile(w io.Writer) {
	l.mu.Lock()
	l.file, l.tried = w, true
	l.mu.Unlock()
}

// openClientLog appends to the service log under the configured root. Other
// processes append to the same file (O_APPEND), so the client never rotates.
func openClientLog(ctrl Control) io.Writer {
	root, err := ConfiguredRoot(ctrl)
	if err != nil {
		return nil
	}
	lf, err := OpenLog(root)
	if err != nil {
		return nil
	}
	lf.Max = math.MaxInt64
	return lf
}

func (c *Client) infof(format string, a ...any) {
	c.lg.write("", false, func() io.Writer { return openClientLog(c.opt.Control) }, format, a...)
}

// warnf: something real happened (possible data loss, a degraded mode) but
// the call goes on. File and stderr.
func (c *Client) warnf(format string, a ...any) {
	c.lg.write("warning: ", true, func() io.Writer { return openClientLog(c.opt.Control) }, format, a...)
}

func (c *Client) errorf(format string, a ...any) {
	c.lg.write("error: ", true, func() io.Writer { return openClientLog(c.opt.Control) }, format, a...)
}
