// Comment commands through the GitHub App webhook (§20.4.7, protocols
// §2.7d): the App's installation token is what verifies the author's rights
// — the payload alone proves nothing.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeGithubAppIssueComment processes one issue_comment delivery.
const TypeGithubAppIssueComment = "githubapp.issue_comment"

// GithubAppIssueComment is the worker handler.
type GithubAppIssueComment struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute fans the comment out to every application bound to the repository,
// exactly like the pull_request job — same ownership chain, same policies.
func (h *GithubAppIssueComment) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
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
	event, err := gitwebhook.ParseComment(gitwebhook.GitHub, delivery.Payload)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	_ = json.Unmarshal(delivery.Payload, &raw)

	finish := func(status store.WebhookDeliveryStatus, reason string) {
		params := store.FinishWebhookDeliveryParams{ID: delivery.ID, Status: status}
		if reason != "" {
			params.IgnoreReason = &reason
		}
		_ = h.Store.FinishWebhookDelivery(ctx, params)
	}

	rec.Start(ctx, "commands")
	if !event.OnPullRequest || event.Command() == "" {
		finish(store.WebhookDeliveryStatusIgnored, "no command on a pull request")
		rec.Skip(ctx, "commands", "no command")
		return map[string]any{"status": "ignored"}, nil
	}

	appIDs, err := h.Store.ListApplicationIDsForRepositoryPush(ctx, store.ListApplicationIDsForRepositoryPushParams{
		GithubAppID: &payload.GithubAppID,
		ExternalID:  fmt.Sprint(raw.Repository.ID),
	})
	if err != nil {
		return nil, err
	}

	// One rights check per delivery, not per application: the author's
	// permission is a property of the repository.
	rights, err := h.installationRights(ctx, payload.GithubAppID, event.AuthorUsername)
	if err != nil {
		h.Logger.Warn("github app comment: rights source unavailable", "error", err)
	}

	results := map[string]string{}
	for _, appID := range appIDs {
		app, err := h.Store.GetApplicationByID(ctx, appID)
		if err != nil {
			continue
		}
		appUUID := pguuid.String(app.Resource.Uuid)
		outcome, err := handlePreviewComment(ctx, h.Store, h.Keyring, h.Logger,
			app, store.GitProviderGithub, event, rights)
		switch {
		case err != nil:
			results[appUUID] = "failed: " + err.Error()
		case outcome.Ignored != "":
			results[appUUID] = "ignored: " + outcome.Ignored
		default:
			results[appUUID] = outcome.Accepted
		}
	}

	if len(results) == 0 {
		finish(store.WebhookDeliveryStatusIgnored, "no application bound to this repository")
	} else {
		finish(store.WebhookDeliveryStatusAccepted, "")
	}
	rec.Succeed(ctx, fmt.Sprintf("%d application(s)", len(results)))
	return map[string]any{"command": event.Command(), "pr": event.Number, "applications": results}, nil
}

// installationRights mints an installation token and returns the author
// rights check for it — nil when the App's credentials are unusable, which
// downstream reports as no_api_credentials.
func (h *GithubAppIssueComment) installationRights(ctx context.Context, githubAppID int64, username string) (func(ctx context.Context, repo string) (bool, error), error) {
	gh, err := h.Store.GetGithubAppByID(ctx, githubAppID)
	if err != nil {
		return nil, err
	}
	if gh.AppID == nil || gh.InstallationID == nil || gh.AppPrivateKeyEnc == nil {
		return nil, nil
	}
	pem, err := h.Keyring.Decrypt("github_apps", "app_private_key_enc", pguuid.String(gh.Uuid), gh.AppPrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	client := &githubapp.Client{APIURL: gh.ApiUrl}
	tokens := githubapp.NewTokenSource(client, *gh.AppID, pem)
	tokenCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := tokens.Token(tokenCtx, *gh.InstallationID, nil)
	if err != nil {
		return nil, err
	}
	return githubAppRights(client, token, username), nil
}
