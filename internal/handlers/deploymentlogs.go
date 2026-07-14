package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// ansiEscapes neutralizes terminal control sequences in log output (§23.3).
var ansiEscapes = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|[\x00-\x08\x0b\x0c\x0e-\x1f]`)

// logLines derives the LogLine stream from the deployment steps: a system
// line per step transition, plus the captured command output. Sequences
// are deterministic, so they work as cursors and Last-Event-ID.
func logLines(steps []store.DeploymentStep) []api.LogLine {
	var lines []api.LogLine
	push := func(ts pgtype.Timestamptz, channel api.LogLineChannel, message string) {
		t := time.Now().UTC()
		if ts.Valid {
			t = ts.Time.UTC()
		}
		lines = append(lines, api.LogLine{
			Sequence:  len(lines) + 1,
			Timestamp: t,
			Channel:   channel,
			Message:   ansiEscapes.ReplaceAllString(message, ""),
		})
	}
	for _, s := range steps {
		if s.Status == store.DeploymentStepStatusSkipped {
			push(s.StartedAt, api.System, fmt.Sprintf("step %s: skipped", s.Name))
		} else {
			push(s.StartedAt, api.System, fmt.Sprintf("step %s: started", s.Name))
		}
		if s.Log != nil {
			for _, line := range strings.Split(strings.TrimRight(*s.Log, "\n"), "\n") {
				push(s.FinishedAt, api.Stdout, line)
			}
		}
		if s.Status == store.DeploymentStepStatusFailed || s.Status == store.DeploymentStepStatusSucceeded {
			push(s.FinishedAt, api.System, fmt.Sprintf("step %s: %s", s.Name, s.Status))
		}
	}
	return lines
}

// GetDeploymentLogs implements GET /deployments/{uuid}/logs (permission:
// read): JSON page by default, SSE stream with text/event-stream (§27.24).
func (a *API) GetDeploymentLogs(w http.ResponseWriter, r *http.Request, deploymentUuid api.DeploymentUuid, params api.GetDeploymentLogsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(deploymentUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
		return
	}
	row, err := a.Store.GetDeploymentByUUIDForTeam(r.Context(), store.GetDeploymentByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "deployment not found")
		return
	}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		a.streamDeploymentLogs(w, r, row.Deployment, params)
		return
	}

	steps, err := a.Store.ListDeploymentSteps(r.Context(), row.Deployment.ID)
	if err != nil {
		a.internalError(w, r, "deployment logs", err)
		return
	}
	lines := logLines(steps)

	after := 0
	if params.Cursor != nil && *params.Cursor != "" {
		if n, err := strconv.Atoi(*params.Cursor); err == nil {
			after = n
		} else {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid cursor")
			return
		}
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}

	page := make([]api.LogLine, 0, limit)
	for _, line := range lines {
		if line.Sequence > after && len(page) < int(limit) {
			page = append(page, line)
		}
	}
	var next *string
	if len(page) == int(limit) && page[len(page)-1].Sequence < len(lines) {
		next = ptr(strconv.Itoa(page[len(page)-1].Sequence))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.LogLine `json:"data"`
		NextCursor *string       `json:"next_cursor"`
	}{page, next})
}

// streamDeploymentLogs emits the SSE representation: event `log` with
// id=sequence, then a final `end` event on terminal status. Resumes from
// Last-Event-ID.
func (a *API) streamDeploymentLogs(w http.ResponseWriter, r *http.Request, d store.Deployment, params api.GetDeploymentLogsParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	last := 0
	if params.LastEventID != nil {
		if n, err := strconv.Atoi(*params.LastEventID); err == nil {
			last = n
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		steps, err := a.Store.ListDeploymentSteps(r.Context(), d.ID)
		if err != nil {
			return
		}
		for _, line := range logLines(steps) {
			if line.Sequence <= last {
				continue
			}
			data, _ := json.Marshal(line)
			_, _ = fmt.Fprintf(w, "event: log\nid: %d\ndata: %s\n\n", line.Sequence, data)
			last = line.Sequence
		}
		flusher.Flush()

		current, err := a.Store.GetDeploymentByID(r.Context(), d.ID)
		if err == nil {
			switch current.Status {
			case store.DeploymentStatusSucceeded, store.DeploymentStatusFailed, store.DeploymentStatusCancelled, store.DeploymentStatusSuperseded:
				_, _ = fmt.Fprintf(w, "event: end\ndata: {\"status\":%q}\n\n", current.Status)
				flusher.Flush()
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
