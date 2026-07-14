// Package uptime implements the integrated uptime monitoring of ADR-017:
// simple HTTP/TCP probes run from the control plane — outside the monitored
// workload, which is the whole point: a struggling server must not be the
// one vouching for its own workloads — and a threshold-based state machine
// whose transitions ARE the anti-flapping.
//
// The scope stops at up/down and latency (no APM, ADR-017).
package uptime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Status is the check verdict. It only flips after enough CONSECUTIVE
// results: a single blip never pages anyone, a single success never clears
// a real outage.
type Status string

const (
	StatusUnknown Status = "unknown"
	StatusUp      Status = "up"
	StatusDown    Status = "down"
)

// Result is one probe execution.
type Result struct {
	OK         bool
	LatencyMs  int32
	StatusCode int32 // http only; 0 otherwise
	Error      string
}

// Probe runs one check. kind is http or tcp; target is a URL or host:port.
func Probe(ctx context.Context, kind, target string, timeout time.Duration) Result {
	start := time.Now()
	done := func(ok bool, code int32, errMsg string) Result {
		return Result{OK: ok, LatencyMs: int32(time.Since(start).Milliseconds()), StatusCode: code, Error: errMsg}
	}
	switch kind {
	case "tcp":
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return done(false, 0, err.Error())
		}
		_ = conn.Close()
		return done(true, 0, "")
	default: // http
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return done(false, 0, err.Error())
		}
		req.Header.Set("User-Agent", "akerdock-uptime/1")
		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			return done(false, 0, err.Error())
		}
		defer func() { _ = resp.Body.Close() }()
		// Up = the endpoint answers something an operator would call alive:
		// 2xx and 3xx. A 500 is reachable but NOT up — that is exactly the
		// outage this feature exists to catch.
		if resp.StatusCode >= 400 {
			return done(false, int32(resp.StatusCode), fmt.Sprintf("HTTP %d", resp.StatusCode))
		}
		return done(true, int32(resp.StatusCode), "")
	}
}

// State is the persisted counters of one check.
type State struct {
	Status               Status
	ConsecutiveFailures  int32
	ConsecutiveSuccesses int32
}

// Transition applies one result to the state machine. changed is true only
// when the VERDICT flips (threshold crossed) — that is the moment worth an
// alert, never the individual probe.
//
// From `unknown`, a single success is enough to establish `up` (a freshly
// created check should not stay grey for success_threshold probes), but a
// failure still needs the full threshold to declare `down`: the first
// verdict on a broken target must be as deliberate as a later outage.
func Transition(s State, ok bool, failureThreshold, successThreshold int32) (State, bool) {
	if ok {
		s.ConsecutiveSuccesses++
		s.ConsecutiveFailures = 0
		if s.Status == StatusUp {
			return s, false
		}
		if s.Status == StatusUnknown || s.ConsecutiveSuccesses >= successThreshold {
			s.Status = StatusUp
			return s, true
		}
		return s, false
	}
	s.ConsecutiveFailures++
	s.ConsecutiveSuccesses = 0
	if s.Status == StatusDown {
		return s, false
	}
	if s.ConsecutiveFailures >= failureThreshold {
		s.Status = StatusDown
		return s, true
	}
	return s, false
}
