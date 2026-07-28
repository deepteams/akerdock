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
	"strings"
	"time"

	"github.com/coder/websocket"
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
	// WSCooldown is how long the agent stays on the POST fallback after a
	// WebSocket failure before re-dialing (ADR-041 §4).
	WSCooldown time.Duration
	// DisableWS forces the POST fallback (tests, or an egress known to break
	// WebSockets).
	DisableWS bool
	now       func() time.Time

	// Channel state — owned by the flush loop, never shared.
	ws        *websocket.Conn
	wsSeq     int64
	wsRetryAt time.Time
}

// NewAgent builds an agent; events may be nil (no Docker stream).
func NewAgent(cfg AgentConfig, events eventStreamer, logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		cfg:        cfg,
		events:     events,
		logger:     logger,
		http:       &http.Client{Timeout: 10 * time.Second},
		queue:      make(chan Observation, agentQueueCap),
		Heartbeat:  time.Minute,
		Flush:      2 * time.Second,
		Backoff:    5 * time.Second,
		WSCooldown: time.Minute,
		now:        time.Now,
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
	// An immediate hello heartbeat opens the channel right away: presence is
	// the connection (ADR-041 §2), so it must not wait a full heartbeat
	// interval after the helper starts.
	a.Push(Observation{Type: "heartbeat", At: a.now()})
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
			err := a.send(ctx, batch)
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
				a.closeWS()
				return
			}
			if backoff < time.Minute {
				backoff *= 2
			}
		}
	}
}

// agentSubprotocol names the ADR-041 channel, next to akerdock-tunnel-v1.
const agentSubprotocol = "akerdock-agent-v1"

// wsFrame is one message on the channel, both directions: the agent sends
// observations, the control plane acknowledges the sequence (Denied marks a
// batch the control plane refuses — dropped like a POST 4xx, not retried).
type wsFrame struct {
	Type         string        `json:"type"`
	Seq          int64         `json:"seq"`
	Observations []Observation `json:"observations,omitempty"`
	Denied       bool          `json:"denied,omitempty"`
}

// send delivers one batch: over the persistent WebSocket when it is up (or
// due a re-dial), over the phase-1 POST otherwise — the ADR-041 degradation
// ladder. A socket failure closes it, arms the cooldown and falls straight
// through to the POST for THIS batch: delivery never waits on a transport.
func (a *Agent) send(ctx context.Context, batch []Observation) error {
	if !a.DisableWS {
		if a.ws == nil && a.now().After(a.wsRetryAt) {
			if err := a.dialWS(ctx); err != nil {
				a.wsRetryAt = a.now().Add(a.WSCooldown)
				a.logger.Warn("agent: channel dial failed — POST fallback", "error", err)
			}
		}
		if a.ws != nil {
			err := a.sendWS(ctx, batch)
			if err == nil {
				return nil
			}
			var deny *denyError
			if errors.As(err, &deny) {
				return err
			}
			a.closeWS()
			a.wsRetryAt = a.now().Add(a.WSCooldown)
			a.logger.Warn("agent: channel broke — POST fallback", "error", err)
		}
	}
	return a.post(ctx, batch)
}

func (a *Agent) dialWS(ctx context.Context) error {
	url := strings.Replace(a.cfg.InstanceURL, "http", "ws", 1) + "/agent/v1/ws"
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		Subprotocols: []string{agentSubprotocol},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + a.cfg.Token}},
	})
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)
	a.ws = conn
	a.logger.Info("agent: channel connected", "instance", a.cfg.InstanceURL)
	return nil
}

// sendWS writes the batch as a frame and waits for its acknowledgement — one
// batch in flight at a time, so at-least-once needs no bookkeeping beyond the
// sequence number.
func (a *Agent) sendWS(ctx context.Context, batch []Observation) error {
	a.wsSeq++
	frame, err := json.Marshal(wsFrame{Type: "observations", Seq: a.wsSeq, Observations: batch})
	if err != nil {
		return err
	}
	ioCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.ws.Write(ioCtx, websocket.MessageText, frame); err != nil {
		return err
	}
	for {
		_, data, err := a.ws.Read(ioCtx)
		if err != nil {
			return err
		}
		var ack wsFrame
		if json.Unmarshal(data, &ack) != nil || ack.Type != "ack" || ack.Seq != a.wsSeq {
			continue
		}
		if ack.Denied {
			return &denyError{status: http.StatusBadRequest}
		}
		return nil
	}
}

func (a *Agent) closeWS() {
	if a.ws != nil {
		_ = a.ws.Close(websocket.StatusNormalClosure, "")
		a.ws = nil
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
	case resp.StatusCode == http.StatusAccepted:
		// The ingestion endpoint answers 202 and nothing else. A generic 200
		// is NOT a success: a misrouted request landing on the SPA answers
		// 200 with HTML, and treating it as delivered silently drops every
		// observation — exactly the bug this guard is here to surface.
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return &denyError{status: resp.StatusCode}
	default:
		return fmt.Errorf("agent push: unexpected %s", resp.Status)
	}
}
