package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	containertypes "github.com/docker/docker/api/types/container"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// GetApplicationMetrics implements GET /applications/{application_uuid}/metrics
// (permission: read): a live CPU/RAM snapshot per compose service, read on
// demand through the agent channel and never stored (ADR-034). Empty for
// non-compose build packs.
func (a *API) GetApplicationMetrics(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermMetricsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	a.writeComponentMetrics(w, r, row.ServerRowID, row.Resource.ID, uuidString(row.Resource.Uuid))
}

// GetPreviewMetrics implements GET
// /applications/{application_uuid}/previews/{preview_uuid}/metrics: the same
// live snapshot for a preview instance's containers (INV-011).
func (a *API) GetPreviewMetrics(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermPreviewsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	preview, ok := a.resolvePreview(w, r, id, row.Resource.ID, previewUuid)
	if !ok {
		return
	}
	if preview.Status == store.PreviewStatusDestroyed || preview.Status == store.PreviewStatusDestroying {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this preview is destroyed")
		return
	}
	// Preview containers derive from the PREVIEW uuid (INV-011).
	a.writeComponentMetrics(w, r, row.ServerRowID, row.Resource.ID, uuidString(preview.Uuid))
}

// writeComponentMetrics reads one stats snapshot per component through the
// agent channel (ADR-052) and maps it onto the resource's compose services
// (container `<base>-<service>`).
func (a *API) writeComponentMetrics(w http.ResponseWriter, r *http.Request, serverID, resourceID int64, base string) {
	components, err := a.Store.ListServiceComponents(r.Context(), resourceID)
	if err != nil {
		a.internalError(w, r, "metrics", err)
		return
	}
	rt, ok := a.agentRuntime(w, r, serverID)
	if !ok {
		return
	}

	type target struct{ container, component string }
	// Single-container build pack (docker image / dockerfile / nixpacks /
	// static): the container IS the resource uuid (INV-011), reported under
	// an empty component name — "the app itself".
	targets := []target{{base, ""}}
	if len(components) > 0 {
		targets = targets[:0]
		for _, c := range components {
			targets = append(targets, target{base + "-" + c.Name, c.Name})
		}
	}
	// One snapshot per container, in parallel: a stream=false stats costs the
	// daemon a ~1.5 s CPU sampling window, and the channel multiplexes — a
	// five-service stack must not answer five windows late.
	out := make([]api.ComponentMetric, len(targets))
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = componentMetric(r.Context(), rt, tgt.container, tgt.component)
		}()
	}
	wg.Wait()
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// componentMetric reads one container's live numbers. A missing or stopped
// container yields running=false with nil numbers; an incomputable figure
// stays nil rather than guessed (ADR-034). The stats call uses stream=false —
// the snapshot whose precpu sample makes CPU% computable.
func componentMetric(ctx context.Context, rt dockerruntime.Runtime, container, component string) api.ComponentMetric {
	m := api.ComponentMetric{Component: ptr(component), Running: ptr(false)}
	inspect, err := rt.ContainerInspect(ctx, container)
	if err != nil || inspect.State == nil || !inspect.State.Running {
		return m
	}
	m.Running = ptr(true)
	stats, err := rt.ContainerStats(ctx, container, false)
	if err != nil {
		return m
	}
	defer func() { _ = stats.Body.Close() }()
	var resp containertypes.StatsResponse
	if json.NewDecoder(io.LimitReader(stats.Body, 1<<20)).Decode(&resp) != nil {
		return m
	}
	m.CpuPercent = dockerruntime.CPUPercent(resp)
	m.MemoryBytes, m.MemoryLimitBytes, m.MemoryPercent = dockerruntime.MemoryUsage(resp)
	return m
}
