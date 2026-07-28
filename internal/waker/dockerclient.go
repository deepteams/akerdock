package waker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DockerSocket is the local Docker Engine API socket the waker is given (§8.1).
// The waker only ever calls start/inspect/stop plus the read-only event
// stream (ADR-040) — a minimal client over this socket keeps the static
// binary free of the full Docker SDK (ADR-021).
const DockerSocket = "/var/run/docker.sock"

// SocketDocker talks to the local Docker daemon over its unix socket. It
// implements Docker with exactly three calls: start, inspect and stop.
type SocketDocker struct {
	http    *http.Client
	version string // pinned Engine API version segment, e.g. "v1.45"
}

// NewSocketDocker builds a client bound to socket. version optionally pins the
// Engine API path segment (e.g. "v1.45"); empty — the default — sends
// unversioned requests, which the daemon serves with its own current API
// version. Pinning a version NEWER than the daemon supports makes every call
// fail ("client version too new"), so we do not pin by default.
func NewSocketDocker(socket, version string) *SocketDocker {
	if socket == "" {
		socket = DockerSocket
	}
	return &SocketDocker{
		version: version,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (c *SocketDocker) endpoint(path string) string {
	// The host is ignored (unix socket) but must be a valid http URL. An empty
	// version yields an unversioned path served by the daemon's current version.
	if c.version == "" {
		return "http://docker" + path
	}
	return "http://docker/" + c.version + path
}

// Start starts a container by name or id. 204 = started, 304 = already running.
func (c *SocketDocker) Start(ctx context.Context, container string) error {
	u := c.endpoint("/containers/" + url.PathEscape(container) + "/start")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("waker: docker start %s: %s: %s", container, resp.Status, body)
	}
}

// ContainerEvent is the slice of a Docker event the agent pushes (ADR-040):
// a state transition of an akerdock.managed container.
type ContainerEvent struct {
	Container string // container name
	Action    string // start, die, stop, oom, health_status: healthy, …
	At        time.Time
}

// eventsResponse is the subset of GET /events entries the agent reads.
type eventsResponse struct {
	Type     string `json:"Type"`
	Action   string `json:"Action"`
	TimeNano int64  `json:"timeNano"`
	Actor    struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// StreamEvents follows the daemon's event stream, filtered to container
// events of akerdock.managed containers, and calls handler for each until ctx
// ends or the stream breaks (the caller reconnects with backoff). It uses a
// dedicated timeout-free client: the shared one's 15 s budget would kill the
// stream mid-flight.
func (c *SocketDocker) StreamEvents(ctx context.Context, handler func(ContainerEvent)) error {
	filters := `{"type":["container"],"label":["akerdock.managed=true"]}`
	u := c.endpoint("/events?filters=" + url.QueryEscape(filters))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	stream := &http.Client{Transport: c.http.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("waker: docker events: %s: %s", resp.Status, body)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev eventsResponse
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if ev.Type != "container" {
			continue
		}
		handler(ContainerEvent{
			Container: ev.Actor.Attributes["name"],
			Action:    ev.Action,
			At:        time.Unix(0, ev.TimeNano),
		})
	}
}

// Stop stops a container — the rollback of a failed wake, the same operation
// the control plane's sleep performs, so a half-woken stack does not
// crash-loop while the control plane believes it asleep. 204 = stopped,
// 304 = already stopped.
func (c *SocketDocker) Stop(ctx context.Context, container string) error {
	u := c.endpoint("/containers/" + url.PathEscape(container) + "/stop?t=10")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("waker: docker stop %s: %s: %s", container, resp.Status, body)
	}
}

// inspectResponse is the subset of GET /containers/{id}/json the waker reads.
type inspectResponse struct {
	State struct {
		Running bool `json:"Running"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// Inspect reports the container's running/health state.
func (c *SocketDocker) Inspect(ctx context.Context, container string) (ContainerState, error) {
	u := c.endpoint("/containers/" + url.PathEscape(container) + "/json")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ContainerState{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ContainerState{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ContainerState{}, fmt.Errorf("waker: docker inspect %s: %s: %s", container, resp.Status, body)
	}
	var out inspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ContainerState{}, err
	}
	st := ContainerState{Running: out.State.Running, Health: "none"}
	if out.State.Health != nil && out.State.Health.Status != "" {
		st.Health = out.State.Health.Status
	}
	return st, nil
}
