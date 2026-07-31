package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	containertypes "github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// GetApplicationLogs implements GET /applications/{application_uuid}/logs
// (permission: read): the last lines of the application's running container,
// read through the agent channel (ADR-052) and never stored (§5.7). The
// runtime console is the missing half of debugging: deployment logs stop at
// the switch, and everything the app prints after lands only here.
func (a *API) GetApplicationLogs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.GetApplicationLogsParams) {
	id, ok := a.require(w, r, auth.PermLogsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	lines := 200
	if params.Lines != nil && *params.Lines > 0 && *params.Lines <= 2000 {
		lines = *params.Lines
	}

	// The container carries the resource UUID as its name (INV-011). A
	// compose stack has no container of its own: `component` picks the
	// service, and its container is `<uuid>-<service>` (compose-spec §2.2).
	container := uuidString(row.Resource.Uuid)
	if params.Component != nil && *params.Component != "" {
		components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
		if err != nil {
			a.internalError(w, r, "application logs", err)
			return
		}
		found := false
		for _, c := range components {
			if c.Name == *params.Component {
				found = true
				break
			}
		}
		if !found {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
				fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", *params.Component))
			return
		}
		container = container + "-" + *params.Component
	}

	rt, ok := a.agentRuntime(w, r, row.ServerRowID)
	if !ok {
		return
	}
	out, err := containerLogsSnapshot(r.Context(), rt, container, lines)
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"the application container does not exist on the server — deploy the application first")
			return
		}
		a.internalError(w, r, "application logs", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": containerLogLines(out)})
}

// containerLogsSnapshot reads a container's last lines through the agent
// channel — stdout and stderr merged in arrival order, the CLI's `2>&1`. The
// inspect reads the TTY bit, which decides the log stream's framing.
func containerLogsSnapshot(ctx context.Context, rt dockerruntime.Runtime, container string, lines int) (string, error) {
	inspect, err := rt.ContainerInspect(ctx, container)
	if err != nil {
		return "", err
	}
	tty := inspect.Config != nil && inspect.Config.Tty
	rc, err := rt.ContainerLogs(ctx, container, containertypes.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines),
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var sb strings.Builder
	if err := dockerruntime.Demux(rc, tty, func(chunk string) { sb.WriteString(chunk) }); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// StreamApplicationLogs implements GET /applications/{uuid}/logs/stream — the
// runtime console as Server-Sent Events (ADR-024): the container's follow
// stream piped line by line as `log` events. Powers `akerdock logs -f`.
func (a *API) StreamApplicationLogs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.StreamApplicationLogsParams) {
	id, ok := a.require(w, r, auth.PermLogsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "streaming unsupported")
		return
	}

	container := uuidString(row.Resource.Uuid)
	if params.Component != nil && *params.Component != "" {
		if !a.componentExists(w, r, row.Resource.ID, *params.Component) {
			return
		}
		container = container + "-" + *params.Component
	}
	rt, ok := a.agentRuntime(w, r, row.ServerRowID)
	if !ok {
		return
	}
	// Inspect before the SSE headers: a missing container must answer 409,
	// not an empty event stream. The TTY bit decides the follow's framing.
	inspect, err := rt.ContainerInspect(r.Context(), container)
	if err != nil {
		if dockerruntime.IsNotFound(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
				"the application container does not exist on the server — deploy the application first")
			return
		}
		a.internalError(w, r, "application logs", err)
		return
	}
	tty := inspect.Config != nil && inspect.Config.Tty
	rc, err := rt.ContainerLogs(r.Context(), container, containertypes.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true, Tail: "200",
	})
	if err != nil {
		a.internalError(w, r, "application logs", err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Flush headers now so the client's onopen fires immediately (and buffering
	// proxies release the response) rather than only on the first log line —
	// a quiet container would otherwise leave the stream stuck "connecting".
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Serialize writes: Demux already calls onOutput from one goroutine, but
	// the ANSI cleanup + SSE framing keep the handler self-contained.
	var mu sync.Mutex
	var seq int
	var carry string
	emit := func(chunk string) {
		mu.Lock()
		defer mu.Unlock()
		carry += chunk
		for {
			nl := strings.IndexByte(carry, '\n')
			if nl < 0 {
				break
			}
			line := strings.TrimRight(carry[:nl], "\r")
			carry = carry[nl+1:]
			seq++
			data, _ := json.Marshal(api.LogLine{
				Sequence: seq, Timestamp: time.Now().UTC(),
				Channel: api.LogLineChannelStdout, Message: ansiEscapes.ReplaceAllString(line, ""),
			})
			_, _ = fmt.Fprintf(w, "event: log\nid: %d\ndata: %s\n\n", seq, data)
			flusher.Flush()
		}
	}
	// Follow streams until the client disconnects (ctx cancels) or the
	// container goes away.
	_ = dockerruntime.Demux(rc, tty, emit)
}
