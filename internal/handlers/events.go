package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// keepaliveInterval keeps idle SSE connections open through proxies.
const keepaliveInterval = 20 * time.Second

// StreamEvents implements GET /events (permission: read): the team's SSE
// event stream, fed by the transactional outbox (ADR-024). Resumes from
// Last-Event-ID.
func (a *API) StreamEvents(w http.ResponseWriter, r *http.Request, params api.StreamEventsParams) {
	id, ok := a.require(w, r, auth.PermAuditRead)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "streaming unsupported")
		return
	}

	// Subscribe before replaying, so no event slips between the two.
	stream, cancel := a.Events.Subscribe(id.TeamUUID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Flush the headers (and a priming comment) right away: without this the 200
	// stays in Go's write buffer until the first event or keepalive (up to 20s),
	// so the browser's EventSource never fires `onopen` and the page is stuck on
	// "connecting…" — worse behind a buffering proxy. The comment is ignored by
	// EventSource but releases any intermediary that waits for a first byte.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	last := int64(0)
	if params.LastEventID != nil {
		if n, err := strconv.ParseInt(*params.LastEventID, 10, 64); err == nil {
			last = n
		}
	}
	if last > 0 {
		missed, err := events.Replay(r.Context(), a.Store, id.TeamUUID, last, 500)
		if err == nil {
			for _, ev := range missed {
				writeEvent(w, ev)
				last = ev.Sequence
			}
			flusher.Flush()
		}
	}

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-stream:
			if ev.Sequence <= last {
				continue // already replayed
			}
			writeEvent(w, ev)
			last = ev.Sequence
			flusher.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev events.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", ev.EventType, ev.Sequence, data)
}
