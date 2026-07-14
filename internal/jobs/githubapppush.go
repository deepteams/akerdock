// GitHub App push processing (git-webhook-protocols §2.4): one app-level
// delivery fans out to every application bound to the pushed repository —
// same policies, same coalescing as the per-endpoint webhooks.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeGithubAppPush processes one push delivery of a GitHub App webhook.
const TypeGithubAppPush = "githubapp.push"

// GithubAppPushPayload references the durable delivery and the app.
type GithubAppPushPayload struct {
	DeliveryID   int64 `json:"delivery_id"`
	GithubAppID  int64 `json:"github_app_id"`
	RepositoryID int64 `json:"repository_external_id,string"`
}

// GithubAppPush is the worker handler.
type GithubAppPush struct {
	Store  *store.Queries
	Logger *slog.Logger
}

// Execute fans the push out. Per-application refusals (branch, skip ci,
// watch paths) are recorded per application in the job result, never as
// delivery errors (§1.2).
func (h *GithubAppPush) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload GithubAppPushPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	delivery, err := h.Store.GetWebhookDeliveryByID(ctx, payload.DeliveryID)
	if err != nil {
		return nil, fmt.Errorf("webhook delivery not found: %w", err)
	}
	if !delivery.SignatureValid {
		return nil, fmt.Errorf("refusing to process a delivery with an invalid signature")
	}

	finish := func(status store.WebhookDeliveryStatus, reason string) {
		params := store.FinishWebhookDeliveryParams{ID: delivery.ID, Status: status}
		if reason != "" {
			params.IgnoreReason = &reason
		}
		_ = h.Store.FinishWebhookDelivery(ctx, params)
	}

	rec.Start(ctx, "fan_out")
	push, err := gitwebhook.ParsePush(gitwebhook.GitHub, delivery.Payload)
	if err != nil {
		finish(store.WebhookDeliveryStatusFailed, err.Error())
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	if push.Deleted() {
		finish(store.WebhookDeliveryStatusIgnored, "branch deleted")
		rec.Skip(ctx, "fan_out", "branch deleted")
		return map[string]any{"status": "ignored", "reason": "branch deleted"}, nil
	}

	appIDs, err := h.Store.ListApplicationIDsForRepositoryPush(ctx, store.ListApplicationIDsForRepositoryPushParams{
		GithubAppID: &payload.GithubAppID,
		ExternalID:  fmt.Sprint(payload.RepositoryID),
	})
	if err != nil {
		return nil, err
	}
	if len(appIDs) == 0 {
		finish(store.WebhookDeliveryStatusIgnored, "no application bound to this repository")
		rec.Skip(ctx, "fan_out", "no application bound")
		return map[string]any{"status": "ignored", "reason": "no application bound"}, nil
	}

	results := map[string]string{}
	deployed := 0
	for _, appID := range appIDs {
		app, err := h.Store.GetApplicationByID(ctx, appID)
		if err != nil {
			continue
		}
		appUUID := pguuid.String(app.Resource.Uuid)
		if reason := PushPolicyReason(app, push); reason != "" {
			results[appUUID] = "ignored: " + reason
			continue
		}
		deployment, err := EnqueueWebhookDeployment(ctx, h.Store, h.Logger, app)
		if err != nil {
			results[appUUID] = "failed: " + err.Error()
			continue
		}
		deployed++
		results[appUUID] = "deployment " + pguuid.String(deployment.Uuid)
	}

	status := store.WebhookDeliveryStatusAccepted
	reason := ""
	if deployed == 0 {
		status, reason = store.WebhookDeliveryStatusIgnored, "no application accepted the push"
	}
	finish(status, reason)
	rec.Succeed(ctx, fmt.Sprintf("%d application(s), %d deployment(s)", len(appIDs), deployed))
	return map[string]any{"status": string(status), "applications": results}, nil
}
