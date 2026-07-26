// Rich PR feedback (§20.4.6, protocols §2.7): ONE upserted comment and a
// check run per preview. Everything here is BEST-EFFORT by contract: a
// feedback failure is logged and never fails the deployment it narrates.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// previewCommentBody renders the single upserted PR comment for a state. It is
// the developer's console for a preview, so the live states carry a footer: the
// AkerDock page (URLs of every service, build logs), the inactivity expiry, and
// — when comment commands are on — the commands they can reply with.
func previewCommentBody(app store.GetApplicationByIDRow, preview store.Preview, state, instanceFqdn string) string {
	url := ""
	if preview.Fqdn != nil && *preview.Fqdn != "" {
		url = "https://" + *preview.Fqdn
	}
	var line string
	switch state {
	case "queued":
		line = "⏳ Preview queued for `" + shortSHA(preview.HeadSha) + "`."
	case "deploying":
		line = "🚀 Preview deploying `" + shortSHA(preview.HeadSha) + "`…"
	case "success":
		line = "✅ Preview ready: " + url
		if url == "" {
			line = "✅ Preview deployed (no domain configured)."
		}
	case "failure":
		line = "❌ Preview deployment failed for `" + shortSHA(preview.HeadSha) + "` — see the deployment logs in AkerDock."
	case "destroyed":
		line = "🧹 Preview destroyed."
	case "awaiting_manual_deploy":
		line = "⏸️ Preview reserved for `" + shortSHA(preview.HeadSha) +
			"` — auto-deploy on open is off. Deploy it with `/deploy` or from AkerDock."
	default:
		line = ""
	}
	body := fmt.Sprintf("**AkerDock preview — %s**\n\n%s", app.Resource.Name, line)

	// Footer only while the preview is (or is becoming) live.
	if state == "queued" || state == "deploying" || state == "success" {
		var meta []string
		if instanceFqdn != "" {
			page := fmt.Sprintf("https://%s/applications/%s/previews/%s",
				instanceFqdn, pguuid.String(app.Resource.Uuid), pguuid.String(preview.Uuid))
			meta = append(meta, "[Open in AkerDock]("+page+")", "[Build logs]("+page+"?tab=deployments)")
		}
		if app.Application.PreviewTtlMinutes != nil && *app.Application.PreviewTtlMinutes > 0 {
			meta = append(meta, "expires after "+humanizeMinutes(int(*app.Application.PreviewTtlMinutes))+" of inactivity")
		}
		if len(meta) > 0 {
			body += "\n\n" + strings.Join(meta, " · ")
		}
	}
	if state == "success" && app.Application.PreviewCommentCommandsEnabled {
		body += "\n\n<sub>Reply <code>/rebuild</code> to redeploy, <code>/keep</code> to reset the expiry, <code>/destroy</code> to tear it down.</sub>"
	}
	return body
}

// humanizeMinutes renders a minute count as the coarsest whole unit: 4320 → 3d.
func humanizeMinutes(m int) string {
	switch {
	case m%1440 == 0:
		return fmt.Sprintf("%dd", m/1440)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// instanceFqdn reads the instance FQDN for building dashboard links; "" when
// unset (links are then omitted rather than pointing nowhere).
func (f *PreviewFeedback) instanceFqdn(ctx context.Context) string {
	if f.Store == nil {
		return ""
	}
	if s, err := f.Store.GetInstanceSettings(ctx); err == nil && s.Fqdn != nil {
		return *s.Fqdn
	}
	return ""
}

// PreviewFeedback carries a preview state transition to the PR.
type PreviewFeedback struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Notify updates the PR's single comment and commit status/check run.
// state is one of queued|deploying|success|failure|destroyed. GitHub App
// sources get the rich path (checks, Deployments API); GitLab and Gitea get
// the parity path (commit statuses + the same upserted comment, §20.4.6).
func (f *PreviewFeedback) Notify(ctx context.Context, app store.GetApplicationByIDRow, preview store.Preview, state string) {
	// Bounded on purpose: feedback must never hold a deployment hostage.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	fqdn := f.instanceFqdn(ctx)
	if app.Application.GitSourceID != nil && app.Application.RepositoryID != nil {
		if source, err := f.Store.GetGitSourceByID(ctx, *app.Application.GitSourceID); err == nil && source.GithubAppID != nil {
			f.notifyGithubApp(ctx, source, app, preview, state, fqdn)
			return
		}
	}
	notifier, err := forgeNotifier(ctx, f.Store, f.Keyring, app, preview.Provider)
	if err != nil {
		f.Logger.Warn("preview feedback: forge client unavailable", "error", err)
		return
	}
	if notifier == nil {
		return // no API credential: the preview works, the PR shows nothing (§3)
	}
	notifyForge(ctx, notifier, f.Logger, app, preview, state, fqdn)
}

// notifyGithubApp is the rich GitHub App path (§2.7).
func (f *PreviewFeedback) notifyGithubApp(ctx context.Context, source store.GitSource, app store.GetApplicationByIDRow, preview store.Preview, state, instanceFqdn string) {
	gh, err := f.Store.GetGithubAppByID(ctx, *source.GithubAppID)
	if err != nil || gh.AppID == nil || gh.InstallationID == nil || gh.AppPrivateKeyEnc == nil {
		return
	}
	repo, err := f.Store.GetRepositoryByID(ctx, *app.Application.RepositoryID)
	if err != nil {
		return
	}
	pem, err := f.Keyring.Decrypt("github_apps", "app_private_key_enc", pguuid.String(gh.Uuid), gh.AppPrivateKeyEnc)
	if err != nil {
		f.Logger.Warn("preview feedback: key decrypt failed", "error", err)
		return
	}
	client := &githubapp.Client{APIURL: gh.ApiUrl}
	tokens := githubapp.NewTokenSource(client, *gh.AppID, pem)
	token, err := tokens.Token(ctx, *gh.InstallationID, nil)
	if err != nil {
		f.Logger.Warn("preview feedback: token mint failed", "error", err)
		return
	}

	url := ""
	if preview.Fqdn != nil && *preview.Fqdn != "" {
		url = "https://" + *preview.Fqdn
	}
	body := previewCommentBody(app, preview, state, instanceFqdn)
	marker := fmt.Sprintf("preview-%s-%d", pguuid.String(app.Resource.Uuid), preview.PrID)
	if err := client.UpsertPRComment(ctx, token, repo.FullName, int(preview.PrID), marker, body); err != nil {
		f.Logger.Warn("preview feedback: comment upsert failed", "error", err)
	}

	// Deployments API (§2.7b): materializes the "View deployment" button on
	// the PR. Transient environment — GitHub marks it inactive on its own once
	// the deployment is gone.
	if state == "success" || state == "failure" {
		environment := fmt.Sprintf("preview/pr-%d", preview.PrID)
		if preview.HeadSha != nil && *preview.HeadSha != "" {
			if deploymentID, err := client.CreateDeployment(ctx, token, repo.FullName, *preview.HeadSha, environment); err != nil {
				f.Logger.Warn("preview feedback: deployment creation failed", "error", err)
			} else {
				ghState := "success"
				if state == "failure" {
					ghState = "failure"
				}
				if err := client.CreateDeploymentStatus(ctx, token, repo.FullName, deploymentID, ghState, url); err != nil {
					f.Logger.Warn("preview feedback: deployment status failed", "error", err)
				}
			}
		}
	}

	// Check run (§2.7a): usable as a required status check. Same name and
	// head_sha across transitions — GitHub surfaces the latest.
	if preview.HeadSha == nil || *preview.HeadSha == "" {
		return
	}
	in := githubapp.CheckRunInput{
		Name:    "AkerDock / preview / " + app.Resource.Name,
		HeadSHA: *preview.HeadSha,
	}
	switch state {
	case "queued":
		in.Status = "queued"
	case "deploying":
		in.Status = "in_progress"
	case "success":
		in.Status, in.Conclusion = "completed", "success"
	case "failure":
		in.Status, in.Conclusion = "completed", "failure"
	default:
		return // a destroyed preview does not rewrite its last check
	}
	if _, err := client.CreateCheckRun(ctx, token, repo.FullName, in); err != nil {
		f.Logger.Warn("preview feedback: check run failed", "error", err)
	}
}

func shortSHA(sha *string) string {
	if sha == nil || *sha == "" {
		return "?"
	}
	if len(*sha) > 12 {
		return (*sha)[:12]
	}
	return *sha
}
