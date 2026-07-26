// PR previews of an application (§20.4): listing and the explicit fork
// approval of §20.4.8 — the one gate a fork PR must pass before anything of
// it is built (INV-010).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

func previewToAPI(p store.Preview) api.Preview {
	out := api.Preview{
		Uuid:           ptr(uuidString(p.Uuid)),
		PrId:           ptr(int(p.PrID)),
		Provider:       ptr(string(p.Provider)),
		SourceBranch:   p.SourceBranch,
		HeadSha:        p.HeadSha,
		IsFork:         ptr(p.IsFork),
		ForkApproved:   ptr(p.ForkApprovedAt.Valid),
		Fqdn:           p.Fqdn,
		Status:         ptr(api.PreviewStatus(p.Status)),
		LastDeployedAt: timePtr(p.LastDeployedAt),
		CreatedAt:      timePtr(p.CreatedAt),
	}
	return out
}

// ListApplicationPreviews implements GET /applications/{uuid}/previews.
func (a *API) ListApplicationPreviews(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	previews, err := a.Store.ListPreviewsForApplication(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "list previews", err)
		return
	}
	data := make([]api.Preview, 0, len(previews))
	for _, p := range previews {
		data = append(data, previewToAPI(p))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// ApprovePreviewFork implements POST /applications/{uuid}/previews/{uuid}/approve
// (permission: deploy): the maintainer's explicit yes (§20.4.8). The preview
// is promoted immediately when capacity allows.
func (a *API) ApprovePreviewFork(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(previewUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "preview not found")
		return
	}
	preview, err := a.Store.GetPreviewByUUIDForTeam(r.Context(), store.GetPreviewByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
	if err != nil || preview.ApplicationID != row.Resource.ID {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "preview not found")
		return
	}
	if !preview.IsFork {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this PR is not from a fork — no approval needed")
		return
	}
	if preview.ForkApprovedAt.Valid {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "already approved")
		return
	}
	if rows, err := a.Store.ApprovePreviewFork(r.Context(), store.ApprovePreviewForkParams{ID: preview.ID}); err != nil || rows == 0 {
		a.internalError(w, r, "approve preview", err)
		return
	}
	a.recordAudit(r, id, "preview.fork_approve", "application", row.Resource.Uuid)

	appRow, err := a.Store.GetApplicationByID(r.Context(), row.Resource.ID)
	if err == nil {
		refreshed, err := a.Store.GetPreviewByID(r.Context(), preview.ID)
		if err == nil {
			_, _, _ = jobs.TryPromotePreview(r.Context(), a.Store, a.Logger, appRow, refreshed, false)
			preview = refreshed
		}
	}
	final, err := a.Store.GetPreviewByID(r.Context(), preview.ID)
	if err == nil {
		preview = final
	}
	httpapi.WriteJSON(w, http.StatusAccepted, previewToAPI(preview))
}

// resolvePreview loads a preview by uuid, bound to the team and application.
func (a *API) resolvePreview(w http.ResponseWriter, r *http.Request, id *auth.Identity, appResourceID int64, previewUuid string) (store.Preview, bool) {
	var u pgtype.UUID
	if err := u.Scan(previewUuid); err == nil {
		preview, err := a.Store.GetPreviewByUUIDForTeam(r.Context(), store.GetPreviewByUUIDForTeamParams{Uuid: u, TeamID: id.TeamID})
		if err == nil && preview.ApplicationID == appResourceID {
			return preview, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "preview not found")
	return store.Preview{}, false
}

// DestroyPreview implements DELETE /applications/{uuid}/previews/{uuid}
// (permission: deploy): tears the instance down — containers, volumes,
// networks, routing (§20.4.6). Production is untouched (INV-011); the PR
// stays open and a /deploy or push recreates a fresh instance.
func (a *API) DestroyPreview(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermDeploy)
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
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this preview is already destroyed or being destroyed")
		return
	}
	if err := jobs.EnqueuePreviewDestroy(r.Context(), a.Store, preview); err != nil {
		a.internalError(w, r, "destroy preview", err)
		return
	}
	a.recordAudit(r, id, "preview.destroy", "application", row.Resource.Uuid)
	w.WriteHeader(http.StatusAccepted)
}

// GetPreviewLogs implements GET /applications/{uuid}/previews/{uuid}/logs
// (permission: read): the runtime console of one preview container — the
// missing half of debugging a PR instance, exactly like the application's
// Logs tab but against the preview-scoped containers (INV-011 naming).
func (a *API) GetPreviewLogs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string, params api.GetPreviewLogsParams) {
	id, ok := a.require(w, r, auth.PermRead)
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
	lines := 200
	if params.Lines != nil && *params.Lines > 0 && *params.Lines <= 2000 {
		lines = *params.Lines
	}

	server, err := a.Store.GetServerByID(r.Context(), row.ServerRowID)
	if err != nil {
		a.internalError(w, r, "preview logs", err)
		return
	}
	key, err := a.Store.GetPrivateKeyByID(r.Context(), server.PrivateKeyID)
	if err != nil {
		a.internalError(w, r, "preview logs", err)
		return
	}
	pem, err := a.Keyring.Decrypt("private_keys", "private_key_enc", uuidString(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		a.internalError(w, r, "preview logs", err)
		return
	}
	client, err := sshexec.Dial(r.Context(), server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, jobs.PinnedHostKey(server))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the server is not reachable over SSH right now")
		return
	}
	defer func() { _ = client.Close() }()

	// Preview containers derive from the PREVIEW uuid (INV-011): the stack
	// name for single containers, `<uuid>-<service>` for compose services.
	container := uuidString(preview.Uuid)
	if params.Component != nil && *params.Component != "" {
		components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
		if err != nil {
			a.internalError(w, r, "preview logs", err)
			return
		}
		found := false
		for _, c := range components {
			if c.Name == *params.Component {
				found = true
				break
			}
		}
		if !found {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
				fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", *params.Component))
			return
		}
		container = container + "-" + *params.Component
	}
	res, err := client.Run(r.Context(), fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, container))
	if err != nil {
		a.internalError(w, r, "preview logs", err)
		return
	}
	if res.ExitCode != 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict,
			"the preview container does not exist on the server — the preview may be destroyed or not deployed yet")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": containerLogLines(res.Stdout)})
}

// CreatePreviewTerminalSession implements
// POST /applications/{uuid}/previews/{uuid}/terminal-sessions (permission:
// write): same contract as the application terminal (§5.7, §24.4), targeting
// a container of the PREVIEW instance (INV-011).
func (a *API) CreatePreviewTerminalSession(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string, params api.CreatePreviewTerminalSessionParams) {
	id, ok := a.require(w, r, auth.PermWrite)
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
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this preview is destroyed — there is no container to open a shell in")
		return
	}
	spec := terminalTargetSpec{
		kind:       store.TerminalTargetContainer,
		serverID:   row.ServerRowID,
		resourceID: &row.Resource.ID,
		previewID:  &preview.ID,
		name:       fmt.Sprintf("%s · PR #%d", row.Resource.Name, preview.PrID),
	}
	if params.Component != nil && *params.Component != "" {
		components, err := a.Store.ListServiceComponents(r.Context(), row.Resource.ID)
		if err != nil {
			a.internalError(w, r, "terminal session", err)
			return
		}
		found := false
		for _, c := range components {
			if c.Name == *params.Component {
				found = true
				break
			}
		}
		if !found {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound,
				fmt.Sprintf("unknown component %q — see GET /applications/{uuid}/components", *params.Component))
			return
		}
		spec.component = *params.Component
		spec.name = fmt.Sprintf("%s · PR #%d · %s", row.Resource.Name, preview.PrID, *params.Component)
	}
	a.createTerminalSession(w, r, id, spec)
}

// githubForApplication resolves the application's GitHub App source and
// mints an installation token scoped to its repository. The conflict message
// names the fix — this powers UI actions, not background jobs.
func (a *API) githubForApplication(ctx context.Context, row appRow) (*githubapp.Client, string, string, error) {
	if row.Application.GitSourceID == nil || row.Application.RepositoryID == nil {
		return nil, "", "", fmt.Errorf("this application has no GitHub App source — pull requests are read through the App (§2.2)")
	}
	source, err := a.Store.GetGitSourceByID(ctx, *row.Application.GitSourceID)
	if err != nil || source.GithubAppID == nil {
		return nil, "", "", fmt.Errorf("this application's git source is not a GitHub App")
	}
	app, err := a.Store.GetGithubAppByID(ctx, *source.GithubAppID)
	if err != nil {
		return nil, "", "", fmt.Errorf("the GitHub App of this source no longer exists")
	}
	if app.AppID == nil || app.InstallationID == nil || app.AppPrivateKeyEnc == nil {
		return nil, "", "", fmt.Errorf("the GitHub App is not installed yet — finish the installation first")
	}
	repo, err := a.Store.GetRepositoryByID(ctx, *row.Application.RepositoryID)
	if err != nil {
		return nil, "", "", fmt.Errorf("the application's repository is not known — redeploy once to resync")
	}
	pem, err := a.Keyring.Decrypt("github_apps", "app_private_key_enc", uuidString(app.Uuid), app.AppPrivateKeyEnc)
	if err != nil {
		return nil, "", "", err
	}
	client := &githubapp.Client{APIURL: app.ApiUrl}
	jwt, err := githubapp.AppJWT(*app.AppID, pem, time.Now())
	if err != nil {
		return nil, "", "", err
	}
	var repos []string
	if _, name, ok := strings.Cut(repo.FullName, "/"); ok {
		repos = []string{name}
	}
	token, err := client.InstallationToken(ctx, jwt, *app.InstallationID, repos)
	if err != nil {
		return nil, "", "", fmt.Errorf("github installation token: %w", err)
	}
	return client, token.Token, repo.FullName, nil
}

// ListApplicationPullRequests implements GET /applications/{uuid}/pull-requests
// (permission: read): the repository's open PRs, read live from the provider.
func (a *API) ListApplicationPullRequests(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	client, token, fullName, err := a.githubForApplication(r.Context(), appRow(row))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, err.Error())
		return
	}
	prs, err := client.ListOpenPullRequests(r.Context(), token, fullName)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "listing pull requests failed: "+err.Error())
		return
	}
	data := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		data = append(data, map[string]any{
			"number": pr.Number, "title": pr.Title, "branch": pr.Head.Ref,
			"head_sha": pr.Head.SHA, "is_fork": pr.IsFork(), "draft": pr.Draft,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// KeepPreview implements POST /applications/{uuid}/previews/{uuid}/keep
// (permission: write): the UI counterpart of the /keep comment command — reset
// the inactivity TTL and clear any expiry warning.
func (a *API) KeepPreview(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermWrite)
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
	if err := a.Store.KeepPreviewAlive(r.Context(), preview.ID); err != nil {
		a.internalError(w, r, "keep preview", err)
		return
	}
	a.recordAudit(r, id, "preview.keep", "preview", preview.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// DeployPreviewForPr implements POST /applications/{uuid}/previews
// (permission: deploy): the platform-side /deploy (§20.4.7). The PR is
// re-read from the provider — never trusted from the browser.
func (a *API) DeployPreviewForPr(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	if !row.Application.PreviewsEnabled {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "previews are disabled for this application — enable them in Settings first")
		return
	}
	var body struct {
		PrID int `json:"pr_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PrID < 1 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "pr_id is required")
		return
	}
	client, token, fullName, err := a.githubForApplication(r.Context(), appRow(row))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, err.Error())
		return
	}
	pr, err := client.GetPullRequest(r.Context(), token, fullName, body.PrID)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, fmt.Sprintf("PR #%d not found on %s", body.PrID, fullName))
		return
	}
	if pr.State != "open" {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, fmt.Sprintf("PR #%d is %s — only open PRs deploy previews", pr.Number, pr.State))
		return
	}

	appRow, err := a.Store.GetApplicationByID(r.Context(), row.Resource.ID)
	if err != nil {
		a.internalError(w, r, "deploy preview", err)
		return
	}
	repoRef := fullName
	preview, _, reason, err := jobs.DeployPreviewForPR(r.Context(), a.Store, a.Keyring, a.Logger,
		appRow, store.GitProviderGithub, pr.Number, pr.Head.Ref, pr.Head.SHA, pr.IsFork(), &repoRef)
	if err != nil {
		a.internalError(w, r, "deploy preview", err)
		return
	}
	a.recordAudit(r, id, "preview.deploy", "application", row.Resource.Uuid)
	if reason != "" {
		a.Logger.Info("preview deploy queued with reason", "application", uuidString(row.Resource.Uuid), "pr", pr.Number, "reason", reason)
	}
	if refreshed, err := a.Store.GetPreviewByID(r.Context(), preview.ID); err == nil {
		preview = refreshed
	}
	httpapi.WriteJSON(w, http.StatusAccepted, previewToAPI(preview))
}

// ListPreviewEnvs implements GET /applications/{uuid}/previews/{uuid}/envs
// (permission: read): the EFFECTIVE variables of one PR instance — the
// shared preview set merged with this preview's own overrides, override
// winning per key (INV-010: production values never appear here).
func (a *API) ListPreviewEnvs(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermSecretsRead)
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
	rows, err := a.Store.ListPreviewEnvVars(r.Context(), store.ListPreviewEnvVarsParams{
		ResourceID: row.Resource.ID, PreviewID: &preview.ID,
	})
	if err != nil {
		a.internalError(w, r, "preview envs", err)
		return
	}
	data := make([]api.EnvironmentVariable, 0, len(rows))
	for _, v := range rows {
		data = append(data, a.envToAPI(id, v))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// CreatePreviewEnv implements POST /applications/{uuid}/previews/{uuid}/envs
// (permission: write): a variable dedicated to THIS preview — same key as
// the shared set means this value wins here, and only here.
func (a *API) CreatePreviewEnv(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, previewUuid string) {
	id, ok := a.require(w, r, auth.PermSecretsWrite)
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
	var body api.EnvironmentVariableCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	created, err := a.insertEnvVar(r, row.Resource.ID, body, true, &preview.ID)
	if err != nil {
		a.writeEnvError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, a.envToAPI(id, created))
}
