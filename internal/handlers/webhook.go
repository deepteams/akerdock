package handlers

import (
	"net/http"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// WebhookDeploy implements GET /deploy (permission: deploy) — the generic
// CI webhook (§5.5). Kept for systems that can only emit GET.
func (a *API) WebhookDeploy(w http.ResponseWriter, r *http.Request, params api.WebhookDeployParams) {
	a.webhookDeploy(w, r, params.Uuid, params.Tag, params.Force, nil)
}

// WebhookDeployPost implements POST /deploy — the recommended form; the
// body is ignored and Idempotency-Key deduplicates CI retries.
func (a *API) WebhookDeployPost(w http.ResponseWriter, r *http.Request, params api.WebhookDeployPostParams) {
	a.webhookDeploy(w, r, params.Uuid, params.Tag, params.Force, params.IdempotencyKey)
}

func (a *API) webhookDeploy(w http.ResponseWriter, r *http.Request, uuids, tags *string, force *bool, idempotencyKey *string) {
	id, ok := a.require(w, r, auth.PermDeploy)
	if !ok {
		return
	}
	if (uuids == nil || *uuids == "") && (tags == nil || *tags == "") {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "at least one of uuid or tag is required")
		return
	}

	// Resolve targets: explicit UUIDs plus every application carrying one
	// of the requested tags, deduplicated.
	targets := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			targets = append(targets, u)
		}
	}
	if uuids != nil {
		for _, u := range strings.Split(*uuids, ",") {
			add(strings.TrimSpace(u))
		}
	}
	if tags != nil && *tags != "" {
		names := make([]string, 0, 2)
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				names = append(names, t)
			}
		}
		rows, err := a.Store.ListApplicationsByTags(r.Context(), store.ListApplicationsByTagsParams{
			TeamID: id.TeamID, TagNames: names,
		})
		if err != nil {
			a.internalError(w, r, "webhook deploy", err)
			return
		}
		for _, u := range rows {
			add(uuidString(u))
		}
	}
	if len(targets) == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "no resource matches the requested uuid or tag")
		return
	}

	forceRebuild := force != nil && *force
	results := make([]api.WebhookDeployResult, 0, len(targets))
	for _, target := range targets {
		row, ok := a.resolveApplication(w, r, id, target)
		if !ok {
			return // the uniform 404 was already written
		}
		// Webhook deployments are coalescable: a queued one for the same
		// application is superseded by this newer push (§3.4).
		deployment, err := a.enqueueDeploymentWith(r, id, appRow(row), forceRebuild, idempotencyKey, store.DeploymentTriggerWebhook)
		if err != nil {
			if err == errQueueFull {
				w.Header().Set("Retry-After", "30")
				httpapi.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "the server deployment queue is full — retry later (§5.5)")
				return
			}
			a.internalError(w, r, "webhook deploy", err)
			return
		}
		a.recordAudit(r, id, "deployment.trigger", "deployment", deployment.Uuid)
		results = append(results, api.WebhookDeployResult{
			ResourceUuid:   uuidString(row.Resource.Uuid),
			DeploymentUuid: uuidString(deployment.Uuid),
		})
	}

	httpapi.WriteJSON(w, http.StatusAccepted, api.WebhookDeployAccepted{Deployments: results})
}
