// Agent loop (ADR-040 phase 1): the helper pushes outbound observations —
// Docker state transitions of managed containers, scale-to-zero wakes, a
// heartbeat — to the control plane over HTTPS, authenticated by a per-server
// token injected at container creation. The loop is an accelerator, never a
// dependency: it is bounded, best-effort, isolated from the wake path, and
// its silence degrades to the control plane's SSH scans.
package waker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Observation is one pushed fact. Types: "container_state" (a managed
// container changed state), "stz_woken" (a wake started containers),
// "heartbeat" (the agent is alive).
type Observation struct {
	Type         string    `json:"type"`
	At           time.Time `json:"at"`
	Container    string    `json:"container,omitempty"`
	State        string    `json:"state,omitempty"`
	ResourceUUID string    `json:"resource_uuid,omitempty"`
}

// AgentConfig is the enrollment injected by the control plane (ADR-040 §3).
type AgentConfig struct {
	InstanceURL string
	Token       string
}

// Enabled reports whether the enrollment is complete; otherwise the helper
// runs waker-only.
func (c AgentConfig) Enabled() bool { return c.InstanceURL != "" && c.Token != "" }

// eventStreamer is the Docker event source; nil disables the stream (tests).
type eventStreamer interface {
	StreamEvents(ctx context.Context, handler func(ContainerEvent)) error
}

const (
	agentQueueCap = 512
	agentBatchMax = 100
)

// Agent buffers observations and flushes them in batches. Overflow drops the
// OLDEST entries: observations are hints the SSH scans re-derive anyway, and
// the traffic path must never block on the control plane (ADR-040 §7).
type Agent struct {
	cfg    AgentConfig
	events eventStreamer
	logger *slog.Logger
	http   *http.Client
	queue  chan Observation

	Heartbeat time.Duration
	Flush     time.Duration
	Backoff   time.Duration
	now       func() time.Time
}

// NewAgent builds an agent; events may be nil (no Docker stream).
func NewAgent(cfg AgentConfig, events eventStreamer, logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		cfg:       cfg,
		events:    events,
		logger:    logger,
		http:      &http.Client{Timeout: 10 * time.Second},
		queue:     make(chan Observation, agentQueueCap),
		Heartbeat: time.Minute,
		Flush:     2 * time.Second,
		Backoff:   5 * time.Second,
		now:       time.Now,
	}
}

// Push queues an observation, dropping the oldest when full. Never blocks.
func (a *Agent) Push(o Observation) {
	select {
	case a.queue <- o:
		return
	default:
	}
	select { // full: shed the oldest, then try once more
	case <-a.queue:
	default:
	}
	select {
	case a.queue <- o:
	default:
	}
}

// Run drives the stream, the heartbeat and the flusher until ctx ends. Each
// loop is panic-contained: a bug here must never take the wake path down.
func (a *Agent) Run(ctx context.Context) {
	if !a.cfg.Enabled() {
		return
	}
	a.logger.Info("agent: observation push enabled", "instance", a.cfg.InstanceURL)
	if a.events != nil {
		go contain(a.logger, "agent stream", func() { a.streamLoop(ctx) })
	}
	go contain(a.logger, "agent heartbeat", func() { a.heartbeatLoop(ctx) })
	contain(a.logger, "agent flush", func() { a.flushLoop(ctx) })
}

// contain runs fn and turns a panic into a log line instead of a crash.
func contain(logger *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("agent: loop panicked — observations stop, waking continues", "loop", name, "panic", r)
		}
	}()
	fn()
}

// interestingActions are the container transitions worth pushing; everything
// else (exec_*, attach, copy…) is noise.
var interestingActions = map[string]bool{
	"start": true, "die": true, "stop": true, "kill": true, "oom": true,
	"restart": true, "pause": true, "unpause": true, "destroy": true,
	"health_status: healthy": true, "health_status: unhealthy": true,
}

func (a *Agent) streamLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.events.StreamEvents(ctx, func(ev ContainerEvent) {
			if !interestingActions[ev.Action] {
				return
			}
			a.Push(Observation{Type: "container_state", At: ev.At, Container: ev.Container, State: ev.Action})
		})
		if ctx.Err() != nil {
			return
		}
		a.logger.Warn("agent: docker event stream interrupted, reconnecting", "error", err)
		if sleepCtx(ctx, a.Backoff) != nil {
			return
		}
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Push(Observation{Type: "heartbeat", At: a.now()})
		}
	}
}

// flushLoop batches observations (up to agentBatchMax or a Flush window) and
// posts them. Delivery is at-least-once: a retryable failure re-sends the
// same batch with capped backoff; a non-retryable one (4xx) drops it — the
// control plane said no, repeating will not change its mind.
func (a *Agent) flushLoop(ctx context.Context) {
	for {
		var batch []Observation
		select {
		case <-ctx.Done():
			return
		case first := <-a.queue:
			batch = append(batch, first)
		}
		window := time.NewTimer(a.Flush)
	collect:
		for len(batch) < agentBatchMax {
			select {
			case <-ctx.Done():
				window.Stop()
				return
			case o := <-a.queue:
				batch = append(batch, o)
			case <-window.C:
				break collect
			}
		}
		window.Stop()

		backoff := a.Backoff
		for {
			err := a.post(ctx, batch)
			if err == nil {
				break
			}
			var deny *denyError
			if errors.As(err, &deny) {
				a.logger.Warn("agent: batch rejected, dropping", "status", deny.status, "count", len(batch))
				break
			}
			a.logger.Warn("agent: push failed, retrying", "error", err, "count", len(batch))
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			if backoff < time.Minute {
				backoff *= 2
			}
		}
	}
}

// denyError marks a non-retryable control-plane refusal (4xx).
type denyError struct{ status int }

func (e *denyError) Error() string { return fmt.Sprintf("denied: %d", e.status) }

func (a *Agent) post(ctx context.Context, batch []Observation) error {
	body, err := json.Marshal(map[string]any{"observations": batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.InstanceURL+"/agent/v1/observations", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return &denyError{status: resp.StatusCode}
	default:
		return fmt.Errorf("agent push: %s", resp.Status)
	}
}
