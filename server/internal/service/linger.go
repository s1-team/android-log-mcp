package service

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// Linger bounds (issue #3): after its last client disconnects the service
// keeps recording for max(LingerMin, 2 × the devices' log buffer span),
// at most LingerMax, so a client that comes back soon finds no gap, and a
// gap after a longer absence is no longer than what the buffer would have
// dropped anyway. ANDROID_LOG_MCP_LINGER overrides it: a duration ("30m",
// "0" = stop with the last client) or "forever".
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
	how := fmt.Sprintf("default: max(%s, 2 × device log buffer span", LingerMin)
	if span > 0 {
		how += " " + span.Round(time.Second).String()
	} else {
		how += " (not measured yet)"
	}
	return d, fmt.Sprintf("%s (%s), at most %s)", d, how, LingerMax)
}

// StartGrace is the shortest time a service waits for its first client,
// whatever the linger (ANDROID_LOG_MCP_LINGER=0 must not stop a service
// before the client that started it connects).
var StartGrace = 30 * time.Second

func (s *Service) linger() (time.Duration, string) {
	var span time.Duration
	if m := s.Manager(); m != nil {
		span = m.ReplaySpan()
	}
	return lingerFor(span, os.Getenv(LingerEnv))
}

// clientsChanged starts the linger countdown when the last client is gone
// and cancels it when one connects. An in-process service lives as long as
// its MCP client process instead.
func (s *Service) clientsChanged(n int) {
	if s.opt.Embedded != "" {
		return
	}
	d, how := s.linger() // takes s.mu itself
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lingerT != nil {
		s.lingerT.Stop()
		s.lingerT = nil
		s.idleSince = time.Time{}
	}
	if n > 0 {
		return
	}
	if d < 0 {
		return
	}
	if !s.everConnected && d < StartGrace {
		// just started (e.g. by a client that is about to connect): never
		// stop before the first client had a chance to connect
		d, how = StartGrace, fmt.Sprintf("%s before the first client connects", StartGrace)
	}
	s.idleSince, s.idleFor = time.Now(), d
	gen := s.lingerGen + 1
	s.lingerGen = gen
	s.lingerT = time.AfterFunc(d, func() {
		s.mu.Lock()
		still := s.lingerGen == gen && s.clients == 0
		s.mu.Unlock()
		if still {
			go s.Stop(fmt.Sprintf("no client for %s (linger %s)", d, how))
		}
	})
}

func (s *Service) stopLinger() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lingerT != nil {
		s.lingerT.Stop()
		s.lingerT = nil
	}
	s.lingerGen++
}

func (s *Service) lingerText() string {
	if s.opt.Embedded != "" {
		return "none (an in-process service stops with its MCP client; the other clients then start a new one)"
	}
	d, how := s.linger()
	s.mu.Lock()
	idle, idleFor := s.idleSince, s.idleFor
	s.mu.Unlock()
	txt := how + " after the last client disconnects"
	if d < 0 {
		txt = how
	}
	if !idle.IsZero() {
		txt += fmt.Sprintf("; no clients: stopping in %s", (idleFor - time.Since(idle)).Round(time.Second))
	}
	return txt
}
