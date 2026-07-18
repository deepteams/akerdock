package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeWebhookProcess applies the policies of a received Git webhook and, if
// they all pass, triggers a deployment (git-webhook-protocols §1.2, steps 8-11).
const TypeWebhookProcess = "webhook.process"

// WebhookProcessPayload references the persisted delivery — never the payload
// itself, and never a secret.
type WebhookProcessPayload struct {
	DeliveryID int64 `json:"delivery_id"`
}

// WebhookProcess decides whether a push deploys. It runs asynchronously: the
// forge already got its 200, so an "ignored" outcome here is a decision, not a
// delivery failure.
type WebhookProcess struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute applies, in order: signature already verified at reception,
// event type, branch match, auto-deploy, [skip ci], watch paths. Each refusal
// is recorded with its reason — an operator must be able to answer "why did my
// push not deploy?" without reading the code.
func (h *WebhookProcess) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload WebhookProcessPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	delivery, err := h.Store.GetWebhookDeliveryByID(ctx, payload.DeliveryID)
	if err != nil {
		return nil, fmt.Errorf("webhook delivery not found: %w", err)
	}
	ignore := func(reason string) (any, error) {
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusIgnored, IgnoreReason: &reason,
		})
		rec.Skip(ctx, "policies", "ignored: "+reason)
		return map[string]any{"status": "ignored", "reason": reason}, nil
	}

	// A delivery that reached this job has a valid signature — the receiver
	// answers 401 otherwise — but the invariant is worth asserting rather than
	// assuming: this is the code path that deploys.
	if !delivery.SignatureValid {
		return nil, fmt.Errorf("refusing to process a delivery with an invalid signature")
	}
	if delivery.ApplicationID == nil {
		return ignore("no application associated with this endpoint")
	}

	rec.Start(ctx, "policies")
	provider := gitwebhook.Provider(delivery.Provider)
	eventType := deref(delivery.EventType)

	// PR/MR and comment deliveries feed the preview lifecycle (§20.4) — the
	// same one the GitHub App uses, so a GitLab MR obeys exactly the same
	// policies as a GitHub PR (protocols §1.2).
	if gitwebhook.IsPullRequestEvent(provider, eventType) {
		return h.processPullRequest(ctx, delivery, provider, rec, ignore)
	}
	if gitwebhook.IsCommentEvent(provider, eventType) {
		return h.processComment(ctx, delivery, provider, rec, ignore)
	}
	if eventType != "push" {
		return ignore("event " + eventType + " is not a push")
	}

	push, err := gitwebhook.ParsePush(provider, delivery.Payload)
	if err != nil {
		msg := err.Error()
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusFailed, IgnoreReason: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, err
	}
	if push.Deleted() {
		return ignore("branch deleted")
	}

	app, err := h.Store.GetApplicationByID(ctx, *delivery.ApplicationID)
	if err != nil {
		return nil, fmt.Errorf("application vanished: %w", err)
	}

	if reason := PushPolicyReason(app, push); reason != "" {
		return ignore(reason)
	}
	rec.Succeed(ctx, "policies passed, deploying "+push.Commit[:min(12, len(push.Commit))])

	// Same path as an API-triggered webhook deployment, coalescing included: an
	// older queued push is superseded by this newer one (§3.4).
	rec.Start(ctx, "deploy")
	deployment, err := EnqueueWebhookDeployment(ctx, h.Store, h.Logger, app)
	if err != nil {
		msg := err.Error()
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusFailed, IgnoreReason: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, err
	}
	rec.Succeed(ctx, "deployment "+pguuid.String(deployment.Uuid)+" queued")

	_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
		ID: delivery.ID, Status: store.WebhookDeliveryStatusAccepted,
	})
	h.Logger.Info("webhook deployment triggered",
		"app", pguuid.String(app.Resource.Uuid), "branch", push.Branch(), "commit", push.Commit)
	return map[string]any{
		"status":          "accepted",
		"deployment_uuid": pguuid.String(deployment.Uuid),
		"commit":          push.Commit,
	}, nil
}

// PushPolicyReason returns why a push must NOT deploy this application, or
// "" when it should (branch match, auto-deploy, [skip ci], watch paths) —
// shared by the per-endpoint webhooks and the GitHub App fan-out (§1.2).
func PushPolicyReason(app store.GetApplicationByIDRow, push gitwebhook.Push) string {
	// The push must be on the branch the application deploys: a push to a
	// feature branch is not a production deployment.
	branch := "main"
	if app.Application.GitBranch != nil && *app.Application.GitBranch != "" {
		branch = *app.Application.GitBranch
	}
	if push.Branch() != branch {
		return "push on branch " + push.Branch() + ", the application deploys " + branch
	}
	if !app.Application.AutoDeployEnabled {
		return "auto_deploy_disabled"
	}
	if push.SkipRequested() {
		return "skip_ci"
	}
	// watch_paths: in a monorepo, a push that touches nothing this application
	// builds must not rebuild it.
	if app.Application.WatchPaths != nil && *app.Application.WatchPaths != "" {
		if !matchesWatchPaths(push.Files, *app.Application.WatchPaths) {
			return "watch_paths: the push touched no watched path"
		}
	}
	return ""
}

// matchesWatchPaths reports whether any changed file falls under one of the
// configured prefixes (newline- or comma-separated). A `*` suffix is a prefix
// match; anything else must match a path or a directory prefix.
func matchesWatchPaths(files []string, spec string) bool {
	patterns := strings.FieldsFunc(spec, func(r rune) bool {
		return r == '\n' || r == ',' || r == ' '
	})
	for _, file := range files {
		for _, pattern := range patterns {
			pattern = strings.TrimSpace(strings.TrimSuffix(pattern, "*"))
			if pattern == "" {
				continue
			}
			if strings.HasPrefix(file, pattern) {
				return true
			}
		}
	}
	return false
}

// processPullRequest routes a PR/MR delivery of a per-application webhook
// into the shared preview lifecycle (§20.4).
func (h *WebhookProcess) processPullRequest(ctx context.Context, delivery store.WebhookDelivery,
	provider gitwebhook.Provider, rec *queue.StepRecorder, ignore func(string) (any, error),
) (any, error) {
	app, err := h.Store.GetApplicationByID(ctx, *delivery.ApplicationID)
	if err != nil {
		return nil, fmt.Errorf("application vanished: %w", err)
	}
	if !app.Application.PreviewsEnabled {
		return ignore("previews disabled")
	}
	event, err := gitwebhook.ParsePullRequest(provider, delivery.Payload)
	if err != nil {
		msg := err.Error()
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusFailed, IgnoreReason: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, err
	}
	outcome, err := HandlePreviewPREvent(ctx, h.Store, h.Keyring, h.Logger,
		app, store.GitProvider(provider), event)
	if err != nil {
		msg := err.Error()
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusFailed, IgnoreReason: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, err
	}
	_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
		ID: delivery.ID, Status: store.WebhookDeliveryStatusAccepted,
	})
	rec.Succeed(ctx, outcome)
	return map[string]any{"status": "accepted", "action": event.Action, "pr": event.Number, "outcome": outcome}, nil
}

// processComment routes a PR/MR comment delivery to the command handler
// (§20.4.7 — /deploy, /destroy; opt-in per application).
func (h *WebhookProcess) processComment(ctx context.Context, delivery store.WebhookDelivery,
	provider gitwebhook.Provider, rec *queue.StepRecorder, ignore func(string) (any, error),
) (any, error) {
	app, err := h.Store.GetApplicationByID(ctx, *delivery.ApplicationID)
	if err != nil {
		return nil, fmt.Errorf("application vanished: %w", err)
	}
	event, err := gitwebhook.ParseComment(provider, delivery.Payload)
	if err != nil {
		return ignore("unparsable comment payload")
	}
	outcome, err := HandlePreviewComment(ctx, h.Store, h.Keyring, h.Logger,
		app, store.GitProvider(provider), event)
	if err != nil {
		msg := err.Error()
		_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusFailed, IgnoreReason: &msg,
		})
		rec.Fail(ctx, msg)
		return nil, err
	}
	if outcome.Ignored != "" {
		return ignore(outcome.Ignored)
	}
	_ = h.Store.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{
		ID: delivery.ID, Status: store.WebhookDeliveryStatusAccepted,
	})
	rec.Succeed(ctx, outcome.Accepted)
	return map[string]any{"status": "accepted", "outcome": outcome.Accepted}, nil
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

// EnqueueWebhookDeployment creates a webhook-triggered deployment and queues
// it, coalescing any older queued deployment of the same application (§3.4).
//
// It lives here, in the engine, rather than in the HTTP layer, so that a push
// from a forge and a call to /api/v1/deploy take *exactly* the same path —
// including the queue limit and the supersede. A second implementation would
// be a second set of bugs.
func EnqueueWebhookDeployment(ctx context.Context, q *store.Queries, logger *slog.Logger,
	app store.GetApplicationByIDRow,
) (store.Deployment, error) {
	// The application points at a destination (a Docker network), which points
	// at the server.
	dest, err := q.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return store.Deployment{}, err
	}
	server, err := q.GetServerByID(ctx, dest.ServerID)
	if err != nil {
		return store.Deployment{}, err
	}

	active, err := q.CountActiveDeploymentsForServer(ctx, server.ID)
	if err != nil {
		return store.Deployment{}, err
	}
	if active >= int64(server.DeploymentQueueLimit) {
		return store.Deployment{}, fmt.Errorf("the server deployment queue is full (§5.5)")
	}

	u, err := pguuid.New()
	if err != nil {
		return store.Deployment{}, err
	}
	snapshot, _ := json.Marshal(map[string]any{
		"config_version": app.Resource.Version,
		"image":          app.BuildConfig.ImageName,
		"tag":            app.BuildConfig.ImageTag,
	})
	deployment, err := q.CreateDeployment(ctx, store.CreateDeploymentParams{
		Uuid: u, ResourceID: app.Resource.ID, Trigger: store.DeploymentTriggerWebhook,
		ImageName: app.BuildConfig.ImageName, ImageTag: app.BuildConfig.ImageTag,
		ServerID: server.ID, ConfigSnapshot: snapshot,
	})
	if err != nil {
		return store.Deployment{}, err
	}

	superseded, err := q.SupersedeQueuedDeployments(ctx, store.SupersedeQueuedDeploymentsParams{
		ResourceID: app.Resource.ID, SupersededByID: &deployment.ID,
	})
	if err != nil {
		return store.Deployment{}, err
	}
	if len(superseded) > 0 {
		if err := q.CancelJobsForDeployments(ctx, superseded); err != nil {
			return store.Deployment{}, err
		}
		logger.Info("coalesced queued webhook deployments",
			"count", len(superseded), "app_uuid", pguuid.String(app.Resource.Uuid))
	}

	lockKey := "deploy:app:" + pguuid.String(app.Resource.Uuid)
	if _, err := queue.Enqueue(ctx, q, queue.EnqueueOptions{
		Queue:       "deploy",
		Type:        TypeDeploymentRun,
		Payload:     DeploymentRunPayload{DeploymentID: deployment.ID},
		LockKey:     &lockKey,
		TeamID:      ptr(app.Resource.TeamID),
		ResourceID:  ptr(app.Resource.ID),
		MaxAttempts: 1, // a failed deployment attempt is terminal (§21.1)
	}); err != nil {
		return store.Deployment{}, err
	}
	return deployment, nil
}
