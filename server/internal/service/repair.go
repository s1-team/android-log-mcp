package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// logHintTail names the service log (the plain tail of every connect error).
func (c *Client) logHintTail() string {
	return "details: " + c.logHint() + "/service.log; try: android-log-mcp service status; android-log-mcp service stop --force"
}

// fail builds an error whose tail names the log and the manual commands.
func (c *Client) fail(format string, a ...any) error {
	return fmt.Errorf("%s. %s", fmt.Sprintf(format, a...), c.logHintTail())
}

// waitAnswerOrFree waits until the service answers or lets go of the lock
// (true), or timeout / ctx (false).
func (c *Client) waitAnswerOrFree(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ctrl := c.opt.Control
	for {
		if c.probe(ctx) == probeAnswers || !Running(ctrl) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// repairHolder runs with spawn.lock held when the service lock is held and
// the service did not answer. It returns replaced=true when the holder was
// stopped and the caller should start a new service, false and nil when the
// service answers (or let go of the lock) in the meantime.
func (c *Client) repairHolder(ctx context.Context) (replaced bool, err error) {
	ctrl, rt := c.opt.Control, c.opt.Repair
	d := Diagnose(ctrl)
	if !d.HaveState || !d.Alive {
		// starting up, shutting down, or a child that inherited the lock
		if c.waitAnswerOrFree(ctx, c.opt.StartTimeout) {
			return false, nil
		}
		return false, c.fail("an android-log-mcp service holds %s but does not answer on %s (%s)", ctrl.LockPath(), ctrl.Socket(), d.Text(ctrl))
	}
	if c.waitAnswerOrFree(ctx, rt.SilentFor) {
		return false, nil
	}
	d = Diagnose(ctrl)
	if d.Stopping {
		if c.waitAnswerOrFree(ctx, rt.StoppingWait) {
			return false, nil
		}
		return false, c.fail("the android-log-mcp service (pid %d) has been shutting down for more than %s (%s)", d.PID, rt.StoppingWait, d.Text(ctrl))
	}
	if !d.Ours || !d.Strong || d.Embedded {
		return false, c.fail("an android-log-mcp service (pid %d) holds %s but does not answer on %s (%s); it was not stopped automatically", d.PID, ctrl.LockPath(), ctrl.Socket(), d.Text(ctrl))
	}
	if !allowReplace(ctrl, time.Now()) {
		return false, c.fail("the android-log-mcp service (pid %d) did not answer and was already replaced 3 times within a minute; not replacing it again (%s)", d.PID, d.Text(ctrl))
	}
	c.infof("replacing the service (pid %d): it holds the lock but has not answered for %s", d.PID, rt.SilentFor)
	res, err := StopHolder(ctrl, d, StopOptions{Auto: true, TermWait: rt.TermWait, KillWait: rt.KillWait, Killing: func(pid int) {
		c.warnf("the service (pid %d) did not stop gracefully and is being killed; log lines it had not flushed yet may be missing", pid)
	}})
	if err != nil {
		if errors.Is(err, ErrNotOurs) {
			return false, c.fail("an android-log-mcp service (pid %d) holds %s but does not answer; it was not stopped: %v", d.PID, ctrl.LockPath(), err)
		}
		c.errorf("could not replace the service (pid %d): %v", d.PID, err)
		return false, c.fail("the android-log-mcp service (pid %d) does not answer and could not be replaced: %v", d.PID, err)
	}
	if res.Killed {
		c.warnf("the service (pid %d) did not answer for more than %s and was replaced; lines from its last moments may be missing (logcat_status shows gaps)", d.PID, rt.SilentFor)
	} else {
		c.infof("the service (pid %d) stopped; starting a new one", d.PID)
	}
	return true, nil
}

func (c *Client) probe(ctx context.Context) probeState {
	return probe(ctx, c.opt.Control, time.Second, c.opt.Repair.HelloWait)
}
