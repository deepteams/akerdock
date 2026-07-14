// PR preview lifecycle from the GitHub App webhook (§20.4, protocols §2.4):
// opened/synchronize/reopened deploy, closed destroys, forks wait for an
// approval that never injects secrets (INV-010).
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeGithubAppPullRequest processes one pull_request delivery.
const TypeGithubAppPullRequest = "githubapp.pull_request"

// TypePreviewDestroy tears one preview instance down.
const TypePreviewDestroy = "preview.destroy"

// GithubAppPullRequestPayload references the durable delivery.
type GithubAppPullRequestPayload struct {
	DeliveryID  int64 `json:"delivery_id"`
	GithubAppID int64 `json:"github_app_id"`
}

// PreviewDestroyPayload names the preview to remove.
type PreviewDestroyPayload struct {
	PreviewID int64 `json:"preview_id"`
}

// GithubAppPullRequest is the worker handler.
type GithubAppPullRequest struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// prEvent is the subset of the pull_request payload the lifecycle needs.
type prEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Draft  bool `json:"draft"`
		Merged bool `json:"merged"`
		Head   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// Execute drives the preview lifecycle for every application bound to the
// PR's base repository.
func (h *GithubAppPullRequest) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload GithubAppPullRequestPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	delivery, err := h.Store.GetWebhookDeliveryByID(ctx, payload.DeliveryID)
	if err != nil {
		return nil, fmt.Errorf("delivery not found: %w", err)
	}
	if !delivery.SignatureValid {
		return nil, fmt.Errorf("refusing an unverified delivery")
	}
	var event prEvent
	if err := json.Unmarshal(delivery.Payload, &event); err != nil {
		return nil, fmt.Errorf("unparsable pull_request payload: %w", err)
	}

	finish := func(status store.WebhookDeliveryStatus, reason string) {
		params := store.FinishWebhookDeliveryParams{ID: delivery.ID, Status: status}
		if reason != "" {
			params.IgnoreReason = &reason
		}
		_ = h.Store.FinishWebhookDelivery(ctx, params)
	}

	rec.Start(ctx, "previews")
	appIDs, err := h.Store.ListApplicationIDsForRepositoryPush(ctx, store.ListApplicationIDsForRepositoryPushParams{
		GithubAppID: &payload.GithubAppID,
		ExternalID:  fmt.Sprint(event.PullRequest.Base.Repo.ID),
	})
	if err != nil {
		return nil, err
	}

	results := map[string]string{}
	for _, appID := range appIDs {
		app, err := h.Store.GetApplicationByID(ctx, appID)
		if err != nil {
			continue
		}
		appUUID := pguuid.String(app.Resource.Uuid)
		if !app.Application.PreviewsEnabled {
			results[appUUID] = "previews disabled"
			continue
		}
		outcome, err := h.handleForApplication(ctx, app, event)
		if err != nil {
			results[appUUID] = "failed: " + err.Error()
			continue
		}
		results[appUUID] = outcome
	}

	if len(results) == 0 {
		finish(store.WebhookDeliveryStatusIgnored, "no application bound to this repository")
	} else {
		finish(store.WebhookDeliveryStatusAccepted, "")
	}
	rec.Succeed(ctx, fmt.Sprintf("%d application(s)", len(results)))
	return map[string]any{"action": event.Action, "pr": event.Number, "applications": results}, nil
}

func (h *GithubAppPullRequest) handleForApplication(ctx context.Context, app store.GetApplicationByIDRow, event prEvent) (string, error) {
	switch event.Action {
	case "closed":
		preview, err := h.Store.GetPreviewByIdentity(ctx, store.GetPreviewByIdentityParams{
			ApplicationID: app.Resource.ID, Provider: store.GitProviderGithub, PrID: int32(event.Number),
		})
		if err != nil {
			return "no preview to destroy", nil
		}
		if preview.Status == store.PreviewStatusDestroyed {
			return "already destroyed", nil
		}
		if err := EnqueuePreviewDestroy(ctx, h.Store, preview); err != nil {
			return "", err
		}
		return "destroy queued", nil

	case "opened", "synchronize", "reopened", "ready_for_review":
		if event.PullRequest.Draft && app.Application.PreviewExcludeDrafts {
			return "draft excluded (preview_exclude_drafts)", nil
		}
		isFork := event.PullRequest.Head.Repo.ID != 0 &&
			event.PullRequest.Head.Repo.ID != event.PullRequest.Base.Repo.ID
		if isFork && !app.Application.PreviewForkApprovalEnabled {
			// Default policy (INV-010): fork PRs deploy nothing at all.
			return "fork ignored (enable preview_fork_approval_enabled to allow approvals)", nil
		}

		preview, err := h.Store.UpsertPreview(ctx, store.UpsertPreviewParams{
			ApplicationID: app.Resource.ID, Provider: store.GitProviderGithub, PrID: int32(event.Number),
			SourceBranch: &event.PullRequest.Head.Ref, HeadSha: &event.PullRequest.Head.SHA,
			IsFork: isFork,
		})
		if err != nil {
			return "", err
		}
		// The URL and the access credential are settled BEFORE the fork gate:
		// a maintainer decides on a preview they can already see the address
		// of, and an approval arriving later must not find a preview that
		// deploys with no URL at all. Nothing here builds or injects anything.
		if err := h.ensurePreviewScaffolding(ctx, app, &preview); err != nil {
			return "", err
		}
		if isFork && !preview.ForkApprovedAt.Valid {
			return "fork waiting for maintainer approval (INV-010)", nil
		}
		promoted, reason, err := TryPromotePreview(ctx, h.Store, h.Logger, app, preview)
		if err != nil {
			return "", err
		}
		feedback := &PreviewFeedback{Store: h.Store, Keyring: h.Keyring, Logger: h.Logger}
		if !promoted {
			feedback.Notify(ctx, app, preview, "queued")
			return "queued: " + reason, nil
		}
		feedback.Notify(ctx, app, preview, "deploying")
		return "deployment queued", nil
	default:
		return "action " + event.Action + " not handled", nil
	}
}

// ensurePreviewScaffolding gives the preview its URL and the application its
// generated protection credential (§20.4.4) — both once.
func (h *GithubAppPullRequest) ensurePreviewScaffolding(ctx context.Context, app store.GetApplicationByIDRow, preview *store.Preview) error {
	if preview.Fqdn == nil || *preview.Fqdn == "" {
		fqdn, err := h.previewFQDN(ctx, app, int(preview.PrID))
		if err == nil && fqdn != "" {
			_ = h.Store.SetPreviewFqdn(ctx, store.SetPreviewFqdnParams{ID: preview.ID, Fqdn: &fqdn})
			preview.Fqdn = &fqdn
		}
	}
	if app.Application.PreviewProtection != store.PreviewProtectionBasicAuth {
		return nil
	}
	// Generated once per application, in the PREVIEW variable set: readable by
	// the team through the envs API, shared by all its previews.
	password, err := randomToken(18)
	if err != nil {
		return err
	}
	u, err := pguuid.New()
	if err != nil {
		return err
	}
	value := "preview:" + password
	enc, err := h.Keyring.Encrypt("environment_variables", "value_enc", pguuid.String(u), []byte(value))
	if err != nil {
		return err
	}
	_, err = h.Store.CreateGeneratedPreviewEnvVar(ctx, store.CreateGeneratedPreviewEnvVarParams{
		Uuid: u, ResourceID: app.Resource.ID, Key: "AKERDOCK_PREVIEW_BASIC_AUTH", ValueEnc: enc,
	})
	return err
}

// previewFQDN applies the application's URL template (§5.6): {{pr_id}},
// {{domain}} (first configured domain), {{random}}; the server wildcard is
// the fallback when the application has no domain.
func (h *GithubAppPullRequest) previewFQDN(ctx context.Context, app store.GetApplicationByIDRow, prID int) (string, error) {
	template := app.Application.PreviewUrlTemplate
	domain := ""
	if domains, err := h.Store.ListDomainsForApplication(ctx, &app.Resource.ID); err == nil && len(domains) > 0 {
		domain = domains[0].Fqdn
	}
	if domain == "" {
		dest, err := h.Store.GetDestinationByID(ctx, app.Resource.DestinationID)
		if err != nil {
			return "", err
		}
		server, err := h.Store.GetServerByID(ctx, dest.ServerID)
		if err != nil {
			return "", err
		}
		if server.WildcardDomain == nil || *server.WildcardDomain == "" {
			return "", nil // no domain, no wildcard: the preview runs unrouted
		}
		appUUID := pguuid.String(app.Resource.Uuid)
		return fmt.Sprintf("pr-%d-%s.%s", prID, appUUID[:8], *server.WildcardDomain), nil
	}
	random, err := randomToken(3)
	if err != nil {
		return "", err
	}
	out := strings.NewReplacer(
		"{{pr_id}}", fmt.Sprint(prID),
		"{{domain}}", domain,
		"{{random}}", random,
	).Replace(template)
	return strings.ToLower(out), nil
}

func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// TryPromotePreview enqueues the preview deployment when the application has
// capacity (§20.4.3) — shared by the PR job and the scheduler's queue drain.
func TryPromotePreview(ctx context.Context, q *store.Queries, logger *slog.Logger, app store.GetApplicationByIDRow, preview store.Preview) (bool, string, error) {
	if app.Application.PreviewMaxConcurrent != nil {
		live, err := q.CountLivePreviewsForApplication(ctx, app.Resource.ID)
		if err != nil {
			return false, "", err
		}
		if live >= int64(*app.Application.PreviewMaxConcurrent) {
			return false, fmt.Sprintf("concurrency cap reached (%d live)", live), nil
		}
	}

	u, err := pguuid.New()
	if err != nil {
		return false, "", err
	}
	dest, err := q.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return false, "", err
	}
	snapshot, _ := json.Marshal(map[string]any{"config_version": app.Resource.Version, "preview_pr": preview.PrID})
	deployment, err := q.CreateDeployment(ctx, store.CreateDeploymentParams{
		Uuid: u, ResourceID: app.Resource.ID, Trigger: store.DeploymentTriggerPreview,
		ServerID: dest.ServerID, ConfigSnapshot: snapshot,
		PreviewID: &preview.ID, CommitSha: preview.HeadSha,
	})
	if err != nil {
		return false, "", err
	}
	// Its own lock: a preview deploys NEXT TO production, never serialized
	// against it — but two deployments of the same preview are.
	lockKey := "deploy:preview:" + pguuid.String(preview.Uuid)
	if _, err := queue.Enqueue(ctx, q, queue.EnqueueOptions{
		Queue:      "deploy",
		Type:       TypeDeploymentRun,
		Payload:    DeploymentRunPayload{DeploymentID: deployment.ID},
		LockKey:    &lockKey,
		TeamID:     &app.Resource.TeamID,
		ResourceID: &app.Resource.ID,
	}); err != nil {
		return false, "", err
	}
	_ = q.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDeploying})
	logger.Info("preview deployment queued", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
	return true, "", nil
}

// EnqueuePreviewDestroy queues the teardown of one preview.
func EnqueuePreviewDestroy(ctx context.Context, q *store.Queries, preview store.Preview) error {
	lockKey := "deploy:preview:" + pguuid.String(preview.Uuid)
	_, err := queue.Enqueue(ctx, q, queue.EnqueueOptions{
		Queue:   "deploy",
		Type:    TypePreviewDestroy,
		Payload: PreviewDestroyPayload{PreviewID: preview.ID},
		LockKey: &lockKey,
	})
	if err == nil {
		err = q.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDestroying})
	}
	return err
}
