// Comment commands on PR/MR previews (§20.4.7, protocols §2.7d): /deploy and
// /destroy, opt-in per application, with the author's rights verified
// server-side through the forge API. A command without a verifiable author
// is refused — a webhook body is attacker-shaped input, only the forge knows
// who really wrote the comment.
package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/store"
)

// CommentOutcome distinguishes a decision (Ignored, answered 200 and
// recorded with its reason) from an executed command (Accepted).
type CommentOutcome struct {
	Accepted string
	Ignored  string
}

func ignored(reason string) CommentOutcome { return CommentOutcome{Ignored: reason} }

// HandlePreviewComment applies a PR/MR comment to the preview lifecycle —
// shared by the per-application webhooks (GitLab Note Hook, Gitea/GitHub
// issue_comment) and the GitHub App fan-out.
//
// rights is the caller-provided author check when the forge client comes
// from elsewhere (GitHub App installation token); nil means "resolve the
// git source token" (manual webhook path).
func HandlePreviewComment(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger,
	app store.GetApplicationByIDRow, provider store.GitProvider, event gitwebhook.CommentEvent,
) (CommentOutcome, error) {
	notifier, err := forgeNotifier(ctx, q, keyring, app, provider)
	if err != nil {
		return CommentOutcome{}, err
	}
	var rights func(ctx context.Context, repo string) (bool, error)
	if notifier != nil {
		rights = func(ctx context.Context, repo string) (bool, error) {
			return notifier.AuthorCanWrite(ctx, repo, event.AuthorUsername, event.AuthorID)
		}
	}
	return handlePreviewComment(ctx, q, keyring, logger, app, provider, event, rights)
}

// handlePreviewComment is the transport-independent core: rights is however
// the caller can verify write access (git-source token or installation
// token); nil means no credential at all.
func handlePreviewComment(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger,
	app store.GetApplicationByIDRow, provider store.GitProvider, event gitwebhook.CommentEvent,
	rights func(ctx context.Context, repo string) (bool, error),
) (CommentOutcome, error) {
	if !event.OnPullRequest {
		return ignored("not a comment on a pull request"), nil
	}
	command := event.Command()
	if command == "" {
		return ignored("no command in the first line"), nil
	}
	if !app.Application.PreviewCommentCommandsEnabled {
		return ignored("comment commands disabled (preview_comment_commands_enabled)"), nil
	}
	if !app.Application.PreviewsEnabled {
		return ignored("previews disabled"), nil
	}

	preview, err := q.GetPreviewByIdentity(ctx, store.GetPreviewByIdentityParams{
		ApplicationID: app.Resource.ID, Provider: provider, PrID: int32(event.Number),
	})
	if err != nil {
		// A PR the lifecycle never saw has no branch and no SHA to deploy:
		// the command cannot conjure them from a comment payload.
		return ignored("no preview known for this PR"), nil
	}

	repo := event.RepoReference
	if repo == "" && preview.RepoReference != nil {
		repo = *preview.RepoReference
	}
	// The rights check is NOT optional (protocols §2.7d/§3): without a
	// credential the author cannot be verified, so the command is refused —
	// with the reason the operator will go looking for.
	if rights == nil {
		return ignored("no_api_credentials: comment commands need a provider API token"), nil
	}
	canWrite, err := rights(ctx, repo)
	if err != nil {
		return CommentOutcome{}, fmt.Errorf("author rights check failed: %w", err)
	}
	if !canWrite {
		return ignored("author lacks write access"), nil
	}

	switch command {
	case "destroy":
		if preview.Status == store.PreviewStatusDestroyed || preview.Status == store.PreviewStatusDestroying {
			return ignored("preview already destroyed"), nil
		}
		if err := EnqueuePreviewDestroy(ctx, q, preview); err != nil {
			return CommentOutcome{}, err
		}
		return CommentOutcome{Accepted: "destroy queued by /destroy"}, nil

	case "deploy":
		if preview.IsFork && !preview.ForkApprovedAt.Valid {
			// A /deploy is a deployment order, not an approval: the approval
			// ritual stays explicit (INV-010, §20.4.8).
			return ignored("fork waiting for maintainer approval (INV-010)"), nil
		}
		// Revive the identity if the preview was destroyed (same semantics
		// as a PR reopen), then deploy at the recorded SHA.
		revived, err := q.UpsertPreview(ctx, store.UpsertPreviewParams{
			ApplicationID: app.Resource.ID, Provider: provider, PrID: preview.PrID,
			SourceBranch: preview.SourceBranch, HeadSha: preview.HeadSha,
			IsFork: preview.IsFork, RepoReference: preview.RepoReference,
		})
		if err != nil {
			return CommentOutcome{}, err
		}
		if err := ensurePreviewScaffolding(ctx, q, keyring, app, &revived); err != nil {
			return CommentOutcome{}, err
		}
		promoted, reason, err := TryPromotePreview(ctx, q, logger, app, revived)
		if err != nil {
			return CommentOutcome{}, err
		}
		feedback := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
		if !promoted {
			feedback.Notify(ctx, app, revived, "queued")
			return CommentOutcome{Accepted: "queued by /deploy: " + reason}, nil
		}
		feedback.Notify(ctx, app, revived, "deploying")
		return CommentOutcome{Accepted: "deployment queued by /deploy"}, nil

	default:
		return ignored("unknown command"), nil
	}
}

// githubAppRights adapts an installation token to the rights signature used
// by handlePreviewComment.
func githubAppRights(client interface {
	CollaboratorCanWrite(ctx context.Context, token, fullName, username string) (bool, error)
}, token, username string,
) func(ctx context.Context, repo string) (bool, error) {
	return func(ctx context.Context, repo string) (bool, error) {
		return client.CollaboratorCanWrite(ctx, token, repo, username)
	}
}

// DeployPreviewForPR is the platform-side /deploy (§20.4): upserts the
// preview identity from provider data and promotes it under exactly the same
// rules as the comment command — fork approval included (INV-010).
func DeployPreviewForPR(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger,
	app store.GetApplicationByIDRow, provider store.GitProvider, prID int, branch, sha string, isFork bool, repoRef *string,
) (store.Preview, bool, string, error) {
	preview, err := q.UpsertPreview(ctx, store.UpsertPreviewParams{
		ApplicationID: app.Resource.ID, Provider: provider, PrID: int32(prID),
		SourceBranch: &branch, HeadSha: &sha, IsFork: isFork, RepoReference: repoRef,
	})
	if err != nil {
		return store.Preview{}, false, "", err
	}
	if err := ensurePreviewScaffolding(ctx, q, keyring, app, &preview); err != nil {
		return preview, false, "", err
	}
	if preview.IsFork && !preview.ForkApprovedAt.Valid {
		// Not an error: the preview identity exists, the approval button is
		// the next step — the caller surfaces the reason.
		return preview, false, "fork waiting for maintainer approval (INV-010)", nil
	}
	promoted, reason, err := TryPromotePreview(ctx, q, logger, app, preview)
	return preview, promoted, reason, err
}
