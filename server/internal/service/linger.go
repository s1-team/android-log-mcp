package service

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/s1crab/android-log-mcp/server/internal/device"
)

// Per-device linger (issue #11 §6): a started device keeps recording for
// max(LingerMin, 2 × its log buffer span), at most LingerMax, after its own
// last log-tool call, so the next call's backfill covers the time it was
// not recorded. ANDROID_LOG_MCP_LINGER overrides it: a duration ("30m",
// "0" = stop right after each call) or "forever" (the service lifetime).
// Clients connecting or disconnecting play no role in recording.
const (
	LingerMin = 10 * time.Minute
	LingerMax = time.Hour
	LingerEnv = "ANDROID_LOG_MCP_LINGER"
)

// lingerFor is the linger time for a buffer span and the env override
// (-1 = forever). The second result describes how it was chosen.
func lingerFor(span time.Duration, env string) (time.Duration, string) {
	env = strings.TrimSpace(env)
	if env != "" {
		if strings.EqualFold(env, "forever") || env == "-1" {
			return -1, "forever (" + LingerEnv + "=" + env + ")"
		}
		if d, err := time.ParseDuration(env); err == nil && d >= 0 {
			return d, fmt.Sprintf("%s (%s)", d, LingerEnv)
		}
		if n, err := time.ParseDuration(env + "s"); err == nil && n >= 0 { // "0", "90"
			return n, fmt.Sprintf("%s (%s)", n, LingerEnv)
		}
		log.Printf("%s=%q is not a duration or \"forever\"; using the default", LingerEnv, env)
	}
	d := max(LingerMin, 2*span)
	d = min(d, LingerMax)
	how := fmt.Sprintf("default: max(%s, 2 × the device's log buffer span", LingerMin)
	if span > 0 {
		how += " " + span.Round(time.Second).String()
	} else {
		how += " (not measured yet)"
	}
	return d, fmt.Sprintf("%s (%s), at most %s)", d, how, LingerMax)
}

func deviceLinger(d *device.Device) time.Duration {
	var span time.Duration
	if d != nil {
		span = d.ReplaySpan()
	}
	l, _ := lingerFor(span, os.Getenv(LingerEnv))
	return l
}

// ExitGrace: the service exits this long after there are no clients, no
// active pin and no recorded device (also right after it started, so the
// client that started it has time to connect).
var ExitGrace = 30 * time.Second

// exitTick is how often the exit condition is checked.
var exitTick = time.Second

// lifeLoop stops the service once it has had no clients, no active pin
// and no recorded device for ExitGrace. An in-process service lives as
// long as its MCP client process instead.
func (s *Service) lifeLoop() {
	if s.opt.Embedded != "" {
		return
	}
	t := time.NewTicker(s.exitTick)
	defer t.Stop()
	for {
		select {
		case <-s.stopping:
			return
		case <-t.C:
		}
		if reason, ok := s.checkExit(time.Now()); ok {
			go s.Stop(reason)
			return
		}
	}
}

// checkExit updates the idle-since time; it returns the stop reason once
// the service has been unused for ExitGrace.
func (s *Service) checkExit(now time.Time) (string, bool) {
	busy := s.busyReason(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if busy != "" {
		s.idleSince = time.Time{}
		return "", false
	}
	if s.idleSince.IsZero() {
		s.idleSince = now
	}
	if now.Sub(s.idleSince) >= s.exitGrace {
		return fmt.Sprintf("no client, no pin and no recorded device for %s", s.exitGrace), true
	}
	return "", false
}

// busyReason is why the service stays ("" = nothing keeps it).
func (s *Service) busyReason(now time.Time) string {
	s.mu.Lock()
	n := s.clients
	m := s.m
	s.mu.Unlock()
	switch {
	case n > 0:
		return fmt.Sprintf("%d client(s) connected", n)
	case s.anyActivePin(now):
		return "a recording pin is active"
	case m != nil && !m.Idle():
		return "a device is recorded"
	}
	return ""
}

// lifeText describes the exit rule and the current state.
func (s *Service) lifeText() string {
	if s.opt.Embedded != "" {
		return "stops with its MCP client (in-process); the other clients then start a new one"
	}
	now := time.Now()
	txt := fmt.Sprintf("exits %s after there are no clients, no active pin and no recorded device", s.exitGrace)
	if b := s.busyReason(now); b != "" {
		return txt + "; staying: " + b
	}
	s.mu.Lock()
	idle := s.idleSince
	s.mu.Unlock()
	if !idle.IsZero() {
		txt += fmt.Sprintf("; exiting in %s", max(0, s.exitGrace-now.Sub(idle)).Round(time.Second))
	}
	return txt
}

// lingerText describes the per-device idle time.
func lingerText() string {
	d, how := lingerFor(0, os.Getenv(LingerEnv))
	if d < 0 {
		return "per device: " + how + " (devices never stop by themselves)"
	}
	if d == 0 {
		return "per device: " + how + ": a device stops right after each call (backfill only)"
	}
	return "per device, after its last log tool call: " + how
}
