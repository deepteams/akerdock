// PR preview lifecycle (§20.4): opened/synchronize/reopened deploy, closed
// destroys, forks wait for an approval that never injects secrets (INV-010).
// The same lifecycle serves the GitHub App webhook and the per-application
// manual webhooks (GitLab MR events, Gitea PR events) — one implementation,
// one set of policies (protocols §1.2).

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
	"github.com/deepteams/akerdock/internal/gitwebhook"
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

// PreviewPromotionStore is the persistence boundary shared by the PR worker
// and the scheduler's preview queue drain.
type PreviewPromotionStore interface {
	queue.EnqueueStore
	CountLivePreviewsForApplication(context.Context, int64) (int64, error)
	GetDestinationByID(context.Context, int64) (store.Destination, error)
	CreateDeployment(context.Context, store.CreateDeploymentParams) (store.Deployment, error)
	SupersedeObsoletePreviewDeployments(context.Context, store.SupersedeObsoletePreviewDeploymentsParams) ([]int64, error)
	CancelJobsForDeployments(context.Context, []int64) error
	ListCancellablePreviewDeploymentIDs(context.Context, store.ListCancellablePreviewDeploymentIDsParams) ([]int64, error)
	RequestDeploymentJobCancel(context.Context, int64) (int64, error)
	SetPreviewStatus(context.Context, store.SetPreviewStatusParams) error
}

// PreviewDestroyQueueStore enqueues preview-destroy jobs.
type PreviewDestroyQueueStore interface {
	queue.EnqueueStore
	SetPreviewStatus(context.Context, store.SetPreviewStatusParams) error
}

// GithubAppPullRequest is the worker handler.
type GithubAppPullRequest struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
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
	event, err := gitwebhook.ParsePullRequest(gitwebhook.GitHub, delivery.Payload)
	if err != nil {
		return nil, fmt.Errorf("unparsable pull_request payload: %w", err)
	}
	// The GitHub App path resolves the repository through its own cache, so
	// the payload's base repo id is what the fan-out keys on.
	var raw struct {
		PullRequest struct {
			Base struct {
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	_ = json.Unmarshal(delivery.Payload, &raw)

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
		ExternalID:  fmt.Sprint(raw.PullRequest.Base.Repo.ID),
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
		outcome, err := HandlePreviewPREvent(ctx, h.Store, h.Keyring, h.Logger, app, store.GitProviderGithub, event)
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

// HandlePreviewPREvent applies the preview lifecycle for one application and
// one normalized PR/MR event — shared by the GitHub App fan-out and the
// per-application webhook path so a GitLab MR and a GitHub PR obey exactly
// the same policies (§20.4, ADR-011).
func HandlePreviewPREvent(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger,
	app store.GetApplicationByIDRow, provider store.GitProvider, event gitwebhook.PullRequestEvent,
) (string, error) {
	destroyIfLive := func() (string, error) {
		preview, err := q.GetPreviewByIdentity(ctx, store.GetPreviewByIdentityParams{
			ApplicationID: app.Resource.ID, Provider: provider, PrID: int32(event.Number),
		})
		if err != nil {
			//nolint:nilerr // no row means nothing to destroy: an expected no-op, not an error.
			return "no preview to destroy", nil
		}
		if preview.Status == store.PreviewStatusDestroyed || preview.Status == store.PreviewStatusDestroying {
			return "already destroyed", nil
		}
		if err := EnqueuePreviewDestroy(ctx, q, preview); err != nil {
			return "", err
		}
		return "destroy queued", nil
	}

	switch event.Action {
	case "closed":
		return destroyIfLive()

	case "opened", "synchronize", "reopened", "ready_for_review", "labeled", "unlabeled":
		// Label opt-in (§20.4.7): when configured, the label IS the switch —
		// a PR without it gets nothing, and removing it from a live preview
		// destroys the preview rather than leaving an unlabeled PR served.
		if required := app.Application.PreviewRequireLabel; required != nil && *required != "" {
			if !event.HasLabel(*required) {
				outcome, err := destroyIfLive()
				if err != nil {
					return "", err
				}
				if outcome == "destroy queued" {
					return "required label removed: " + outcome, nil
				}
				return "label " + *required + " required (preview_require_label)", nil
			}
		} else if event.Action == "labeled" || event.Action == "unlabeled" {
			// No label control configured: label noise redeploys nothing.
			return "label events ignored without preview_require_label", nil
		}
		if event.Draft && app.Application.PreviewExcludeDrafts {
			return "draft excluded (preview_exclude_drafts)", nil
		}
		if event.IsFork && !app.Application.PreviewForkApprovalEnabled {
			// Default policy (INV-010): fork PRs deploy nothing at all.
			return "fork ignored (enable preview_fork_approval_enabled to allow approvals)", nil
		}

		var repoRef *string
		if event.RepoReference != "" {
			repoRef = &event.RepoReference
		}
		preview, err := q.UpsertPreview(ctx, store.UpsertPreviewParams{
			ApplicationID: app.Resource.ID, Provider: provider, PrID: int32(event.Number),
			SourceBranch: &event.HeadRef, HeadSha: &event.HeadSHA,
			IsFork: event.IsFork, RepoReference: repoRef,
		})
		if err != nil {
			return "", err
		}
		// A label event on a preview already serving this SHA changes
		// nothing: redeploying it would only churn the instance.
		if event.Action == "labeled" &&
			(preview.Status == store.PreviewStatusActive || preview.Status == store.PreviewStatusDeploying) &&
			preview.HeadSha != nil && *preview.HeadSha == event.HeadSHA {
			return "already deployed at this SHA", nil
		}
		// The URL and the access credential are settled BEFORE the fork gate:
		// a maintainer decides on a preview they can already see the address
		// of, and an approval arriving later must not find a preview that
		// deploys with no URL at all. Nothing here builds or injects anything.
		if err := ensurePreviewScaffolding(ctx, q, keyring, app, &preview); err != nil {
			return "", err
		}
		if event.IsFork && !preview.ForkApprovedAt.Valid {
			return "fork waiting for maintainer approval (INV-010)", nil
		}
		// Manual-first policy (preview_deploy_on_open=false): the webhook never
		// initiates the FIRST deployment — it only reserves the preview (URL and
		// credential are already settled above). A human deploys it from AkerDock
		// or with /deploy; once it is engaged (deploying/active/failed), later
		// pushes keep updating it here as usual.
		if ManualFirstReserved(app, preview) {
			feedback := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
			feedback.Notify(ctx, app, preview, "awaiting_manual_deploy")
			return "awaiting manual deploy (preview_deploy_on_open=false)", nil
		}
		promoted, reason, err := TryPromotePreview(ctx, q, logger, app, preview, false)
		if err != nil {
			return "", err
		}
		feedback := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
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

// PreviewEngaged reports whether a deployment has already been triggered for
// this preview: a fresh row sits at the default 'queued' status with no deploy
// timestamp, while any promotion moves it to deploying/active (or failed on a
// bad build). Used by the manual-first policy to tell "never deployed" (gate
// the webhook) from "already live" (let pushes keep updating it).
func PreviewEngaged(p store.Preview) bool {
	switch p.Status {
	case store.PreviewStatusDeploying, store.PreviewStatusActive, store.PreviewStatusFailed:
		return true
	default:
		return p.LastDeployedAt.Valid
	}
}

// ManualFirstReserved reports whether a preview exists only as a RESERVATION
// under the manual-first policy (preview_deploy_on_open=false): no deployment
// ever ran, and no human ordered one (deploy_requested_at unset). Neither the
// PR webhook nor the scheduler's capacity queue may promote it — the first
// deployment belongs to a human (the Previews tab, /deploy) (§20.4).
func ManualFirstReserved(app store.GetApplicationByIDRow, p store.Preview) bool {
	return !app.Application.PreviewDeployOnOpen && !p.DeployRequestedAt.Valid && !PreviewEngaged(p)
}

// ensurePreviewScaffolding gives the preview its URL and the application its
// generated protection credential (§20.4.4) — both once.
func ensurePreviewScaffolding(ctx context.Context, q *store.Queries, keyring *envelope.Keyring,
	app store.GetApplicationByIDRow, preview *store.Preview,
) error {
	// A stable per-preview slug backs {{random}} across every route and redeploy
	// (ADR-035) — generated once so hostnames never churn.
	if preview.RandomSlug == nil || *preview.RandomSlug == "" {
		if slug, err := randomToken(3); err == nil {
			_ = q.SetPreviewRandomSlug(ctx, store.SetPreviewRandomSlugParams{ID: preview.ID, RandomSlug: &slug})
			preview.RandomSlug = &slug
		}
	}
	if preview.Fqdn == nil || *preview.Fqdn == "" {
		fqdn, err := previewFQDN(ctx, q, app, int(preview.PrID), previewRandom(preview))
		if err == nil && fqdn != "" {
			_ = q.SetPreviewFqdn(ctx, store.SetPreviewFqdnParams{ID: preview.ID, Fqdn: &fqdn})
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
	enc, err := keyring.Encrypt("environment_variables", "value_enc", pguuid.String(u), []byte(value))
	if err != nil {
		return err
	}
	_, err = q.CreateGeneratedPreviewEnvVar(ctx, store.CreateGeneratedPreviewEnvVarParams{
		Uuid: u, ResourceID: app.Resource.ID, Key: "AKERDOCK_PREVIEW_BASIC_AUTH", ValueEnc: enc,
	})
	return err
}

// previewRandom returns the preview's stable {{random}} slug (ADR-035).
func previewRandom(p *store.Preview) string {
	if p.RandomSlug != nil {
		return *p.RandomSlug
	}
	return ""
}

// primaryServiceName is the web service a {{service}} primary host resolves to
// on a compose stack; "" for a single-container app.
func primaryServiceName(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow) string {
	comps, err := q.ListServiceComponents(ctx, app.Resource.ID)
	if err != nil {
		return ""
	}
	if c, err := resolveWebComponent(comps, nil); err == nil {
		return c.Name
	}
	if len(comps) > 0 {
		return comps[0].Name
	}
	return ""
}

// previewFQDN computes the PRIMARY hostname of a preview (§20.4, ADR-035): the
// first explicit route template, else the {{service}} template resolved for the
// web service. The server wildcard is the fallback when a template needs a
// domain the application does not have.
func previewFQDN(ctx context.Context, q *store.Queries, app store.GetApplicationByIDRow, prID int, random string) (string, error) {
	domain := ""
	if domains, err := q.ListDomainsForApplication(ctx, &app.Resource.ID); err == nil && len(domains) > 0 {
		domain = domains[0].Fqdn
	}
	templates := previewTemplates(app)

	wildcard := func() (string, error) {
		dest, err := q.GetDestinationByID(ctx, app.Resource.DestinationID)
		if err != nil {
			return "", err
		}
		server, err := q.GetServerByID(ctx, dest.ServerID)
		if err != nil {
			return "", err
		}
		if server.WildcardDomain == nil || *server.WildcardDomain == "" {
			return "", nil // no domain, no wildcard: the preview runs unrouted
		}
		return fmt.Sprintf("pr-%d-%s.%s", prID, pguuid.String(app.Resource.Uuid)[:8], *server.WildcardDomain), nil
	}

	if len(templates) == 0 {
		return wildcard()
	}
	host, svc := templates[0].Host, ""
	if ex := explicitTemplates(templates); len(ex) > 0 {
		host = ex[0].Host
	} else {
		svc = primaryServiceName(ctx, q, app)
	}
	// A template that needs {{domain}} but has none falls back to the wildcard —
	// otherwise it would resolve to a malformed host.
	if strings.Contains(host, "{{domain}}") && domain == "" {
		return wildcard()
	}
	return resolvePreviewHost(host, prID, domain, svc, random), nil
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
func TryPromotePreview(ctx context.Context, q PreviewPromotionStore, logger *slog.Logger, app store.GetApplicationByIDRow, preview store.Preview, forceRebuild bool) (bool, string, error) {
	_, promoted, reason, err := PromotePreviewDeployment(ctx, q, logger, app, preview, forceRebuild)
	return promoted, reason, err
}

// PromotePreviewDeployment is TryPromotePreview, returning the deployment it
// created — the API answers with its uuid, the background callers ignore it.
func PromotePreviewDeployment(ctx context.Context, q PreviewPromotionStore, logger *slog.Logger, app store.GetApplicationByIDRow, preview store.Preview, forceRebuild bool) (store.Deployment, bool, string, error) {
	if app.Application.PreviewMaxConcurrent != nil {
		live, err := q.CountLivePreviewsForApplication(ctx, app.Resource.ID)
		if err != nil {
			return store.Deployment{}, false, "", err
		}
		if live >= int64(*app.Application.PreviewMaxConcurrent) {
			return store.Deployment{}, false, fmt.Sprintf("concurrency cap reached (%d live)", live), nil
		}
	}

	u, err := pguuid.New()
	if err != nil {
		return store.Deployment{}, false, "", err
	}
	dest, err := q.GetDestinationByID(ctx, app.Resource.DestinationID)
	if err != nil {
		return store.Deployment{}, false, "", err
	}
	snapshot, _ := json.Marshal(map[string]any{"config_version": app.Resource.Version, "preview_pr": preview.PrID})
	deployment, err := q.CreateDeployment(ctx, store.CreateDeploymentParams{
		Uuid: u, ResourceID: app.Resource.ID, Trigger: store.DeploymentTriggerPreview,
		ServerID: dest.ServerID, ConfigSnapshot: snapshot,
		PreviewID: &preview.ID, CommitSha: preview.HeadSha, ForceRebuild: forceRebuild,
	})
	if err != nil {
		return store.Deployment{}, false, "", err
	}
	// Cancel-obsolete (§20.4.7, opt-in): the deployment that was building the
	// previous commit of this PR is now building history. Queued ones are
	// superseded; running ones are cancelled cooperatively — never past the
	// traffic switch (§21.1).
	if app.Application.PreviewCancelObsoleteBuilds {
		superseded, err := q.SupersedeObsoletePreviewDeployments(ctx, store.SupersedeObsoletePreviewDeploymentsParams{
			PreviewID: &preview.ID, SupersededByID: &deployment.ID,
		})
		if err != nil {
			return store.Deployment{}, false, "", err
		}
		if len(superseded) > 0 {
			if err := q.CancelJobsForDeployments(ctx, superseded); err != nil {
				return store.Deployment{}, false, "", err
			}
		}
		running, err := q.ListCancellablePreviewDeploymentIDs(ctx, store.ListCancellablePreviewDeploymentIDsParams{
			PreviewID: &preview.ID, ID: deployment.ID,
		})
		if err != nil {
			return store.Deployment{}, false, "", err
		}
		for _, id := range running {
			if _, err := q.RequestDeploymentJobCancel(ctx, id); err != nil {
				return store.Deployment{}, false, "", err
			}
		}
		if len(superseded) > 0 || len(running) > 0 {
			logger.Info("obsolete preview builds cancelled",
				"preview", pguuid.String(preview.Uuid), "superseded", len(superseded), "cancelling", len(running))
		}
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
		return store.Deployment{}, false, "", err
	}
	_ = q.SetPreviewStatus(ctx, store.SetPreviewStatusParams{ID: preview.ID, Status: store.PreviewStatusDeploying})
	logger.Info("preview deployment queued", "preview", pguuid.String(preview.Uuid), "pr", preview.PrID)
	return deployment, true, "", nil
}

// EnqueuePreviewDestroy queues the teardown of one preview.
func EnqueuePreviewDestroy(ctx context.Context, q PreviewDestroyQueueStore, preview store.Preview) error {
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
