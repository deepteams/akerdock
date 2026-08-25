// Resolution of the degraded feedback client (§20.4.6, protocols §4/§6): a
// GitLab or Gitea application talks to its forge with the API token stored
// on its git source. No token means no feedback and no comment commands —
// the preview itself still works (protocols §3).
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/gitforge"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// forgeHTTPClient overrides the forge HTTP client in tests, whose fake forges
// live on loopback. Nil — always, in production — keeps gitforge's default,
// the SSRF-guarded safedial client (threat model AB-10).
var forgeHTTPClient *http.Client

// forgeNotifier builds the feedback client for the application's git source,
// or nil when the source has no API token (feedback is then skipped — a
// decision, never an error). GitHub App sources use the richer path in
// PreviewFeedback and never come through here.
func forgeNotifier(ctx context.Context, q *store.Queries, keyring *envelope.Keyring,
	app store.GetApplicationByIDRow, provider store.GitProvider,
) (gitforge.Notifier, error) {
	if app.Application.GitSourceID == nil {
		return nil, nil
	}
	source, err := q.GetGitSourceByID(ctx, *app.Application.GitSourceID)
	if err != nil {
		return nil, err
	}
	if source.ApiTokenEnc == nil {
		return nil, nil
	}
	token, err := keyring.Decrypt("git_sources", "api_token_enc", pguuid.String(source.Uuid), source.ApiTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("git source token decrypt failed: %w", err)
	}
	baseURL := ""
	if source.ApiUrl != nil && *source.ApiUrl != "" {
		baseURL = strings.TrimRight(*source.ApiUrl, "/")
	} else {
		baseURL = defaultForgeAPIURL(provider, app.Application.GitRepositoryUrl)
	}
	if baseURL == "" {
		return nil, nil
	}
	switch provider {
	case store.GitProviderGitlab:
		return &gitforge.GitLab{BaseURL: baseURL, Token: string(token), HTTPClient: forgeHTTPClient}, nil
	case store.GitProviderGitea:
		return &gitforge.Gitea{BaseURL: baseURL, Token: string(token), HTTPClient: forgeHTTPClient}, nil
	default:
		// GitHub manual-webhook feedback by PAT is a proposed default the
		// matrix marks optional (protocols §3) — not implemented yet.
		return nil, nil
	}
}

// defaultForgeAPIURL derives the API root from the repository host when the
// git source does not pin one: /api/v4 for GitLab, /api/v1 for Gitea. The
// scheme is always https — an API token over clear HTTP would be a credential
// broadcast (INV-003).
func defaultForgeAPIURL(provider store.GitProvider, repoURL *string) string {
	if repoURL == nil || *repoURL == "" {
		return ""
	}
	host := repoHost(*repoURL)
	if host == "" {
		return ""
	}
	switch provider {
	case store.GitProviderGitlab:
		return "https://" + host + "/api/v4"
	case store.GitProviderGitea:
		return "https://" + host + "/api/v1"
	default:
		return ""
	}
}

// repoHost extracts the bare host from the URL shapes git accepts: https,
// ssh://, and the scp-like git@host:path.
func repoHost(raw string) string {
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil {
			return u.Hostname()
		}
		return ""
	}
	// scp-like: git@host:org/repo.git
	if _, after, ok := strings.Cut(raw, "@"); ok {
		host, _, _ := strings.Cut(after, ":")
		return host
	}
	return ""
}

// previewStatusURL is the target_url of commit statuses: the preview when it
// has one, nothing otherwise.
func previewStatusURL(preview store.Preview) string {
	if preview.Fqdn != nil && *preview.Fqdn != "" {
		return "https://" + *preview.Fqdn
	}
	return ""
}

// notifyForge carries one preview transition to GitLab/Gitea (§20.4.6):
// commit status + THE upserted comment. Best-effort by contract — every
// failure is logged, none is returned.
func notifyForge(ctx context.Context, notifier gitforge.Notifier, logger *slog.Logger,
	app store.GetApplicationByIDRow, preview store.Preview, state, instanceFqdn string,
) {
	repo := ""
	if preview.RepoReference != nil {
		repo = *preview.RepoReference
	}
	if repo == "" {
		logger.Warn("preview feedback: no repo reference on the preview", "preview", pguuid.String(preview.Uuid))
		return
	}
	url := previewStatusURL(preview)

	// The commit-status state; the comment body is the shared rich renderer.
	var status gitforge.StatusState
	switch state {
	case "queued":
		status = gitforge.StatusQueued
	case "deploying":
		status = gitforge.StatusRunning
	case "success":
		status = gitforge.StatusSuccess
	case "failure":
		status = gitforge.StatusFailure
	case "destroyed":
		status = ""
	default:
		return
	}

	body := previewCommentBody(app, preview, state, instanceFqdn)
	marker := fmt.Sprintf("preview-%s-%d", pguuid.String(app.Resource.Uuid), preview.PrID)
	if err := notifier.UpsertComment(ctx, repo, int(preview.PrID), marker, body); err != nil {
		logger.Warn("preview feedback: comment upsert failed", "error", err)
	}
	// A destroyed preview does not rewrite its last commit status (§2.7a).
	if status == "" || preview.HeadSha == nil || *preview.HeadSha == "" {
		return
	}
	if err := notifier.SetCommitStatus(ctx, repo, *preview.HeadSha, status, url); err != nil {
		logger.Warn("preview feedback: commit status failed", "error", err)
	}
}
