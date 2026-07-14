// GitHub Apps (git-webhook-protocols §2): manifest flow, installation,
// repository discovery, and the app-level webhook reception. The credentials
// GitHub returns are encrypted at rest and never rendered by the API
// (INV-003) — what the dashboard sees is identity and installation state.
package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/githubapp"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

func githubAppToAPI(row store.GithubApp) api.GithubApp {
	out := api.GithubApp{
		Uuid:        ptr(uuidString(row.Uuid)),
		Name:        row.Name,
		Slug:        row.Slug,
		ApiUrl:      ptr(row.ApiUrl),
		HtmlUrl:     ptr(row.HtmlUrl),
		IsInstalled: ptr(row.InstallationID != nil),
		CreatedAt:   timePtr(row.CreatedAt),
		Version:     ptr(int(row.Version)),
	}
	if row.AppID != nil {
		id := int(*row.AppID)
		out.AppId = &id
	}
	if row.InstallationID != nil {
		id := int(*row.InstallationID)
		out.InstallationId = &id
	}
	if row.Slug != nil {
		out.InstallUrl = ptr(strings.TrimRight(row.HtmlUrl, "/") + "/apps/" + *row.Slug + "/installations/new")
	}
	return out
}

// instanceBaseURL is where GitHub must call back: the configured FQDN, https.
func (a *API) instanceBaseURL(r *http.Request) (string, bool) {
	settings, err := a.Settings.Get(r.Context())
	if err != nil || settings.Fqdn == nil || *settings.Fqdn == "" {
		return "", false
	}
	return "https://" + *settings.Fqdn, true
}

// CreateGithubApp implements POST /github-apps (permission: write) —
// manifest flow step 1 (§2.1): draft + one-shot state + manifest to submit.
func (a *API) CreateGithubApp(w http.ResponseWriter, r *http.Request) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	var body api.GithubAppCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	base, ok2 := a.instanceBaseURL(r)
	if !ok2 {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("instance_fqdn"), Code: ptr("required"),
			Message: "the instance FQDN must be configured first: GitHub has to reach this instance for the manifest callback and the webhooks",
		}})
		return
	}

	apiURL, htmlURL := "https://api.github.com", "https://github.com"
	if body.ApiUrl != nil && *body.ApiUrl != "" {
		apiURL = *body.ApiUrl
	}
	if body.HtmlUrl != nil && *body.HtmlUrl != "" {
		htmlURL = *body.HtmlUrl
	}
	for field, value := range map[string]string{"api_url": apiURL, "html_url": htmlURL} {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr(field), Code: ptr("invalid"), Message: field + " must be a https URL",
			}})
			return
		}
	}

	name := "akerdock"
	if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
		name = strings.TrimSpace(*body.Name)
	} else if settings, err := a.Settings.Get(r.Context()); err == nil && settings.Fqdn != nil {
		host, _, _ := strings.Cut(*settings.Fqdn, ".")
		name = "akerdock-" + host
	}
	// GitHub caps app names at 34 characters; a random suffix dodges the
	// global-uniqueness collisions of common instance names.
	suffix := make([]byte, 2)
	_, _ = rand.Read(suffix)
	if len(name) > 29 {
		name = name[:29]
	}
	name = name + "-" + hex.EncodeToString(suffix)

	// The state is single-use and short-lived (10 min): only its hash is
	// stored, so a database read never yields a usable callback token.
	rawState := make([]byte, 32)
	if _, err := rand.Read(rawState); err != nil {
		a.internalError(w, r, "create github app", err)
		return
	}
	state := hex.EncodeToString(rawState)
	stateHash := sha256.Sum256([]byte(state))
	hash := hex.EncodeToString(stateHash[:])

	row, err := a.Store.CreateDraftGithubApp(r.Context(), store.CreateDraftGithubAppParams{
		TeamID: id.TeamID, Name: name, ApiUrl: apiURL, HtmlUrl: htmlURL,
		ManifestStateHash: &hash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a GitHub App with this name already exists in this team")
			return
		}
		a.internalError(w, r, "create github app", err)
		return
	}
	a.recordAudit(r, id, "github_app.create", "github_app", row.Uuid)

	target := strings.TrimRight(htmlURL, "/") + "/settings/apps/new?state=" + state
	if body.Organization != nil && *body.Organization != "" {
		target = strings.TrimRight(htmlURL, "/") + "/organizations/" + url.PathEscape(*body.Organization) + "/settings/apps/new?state=" + state
	}
	httpapi.WriteJSON(w, http.StatusCreated, api.GithubAppManifest{
		GithubApp: githubAppToAPI(row),
		Manifest:  githubapp.Manifest(base, uuidString(row.Uuid), name),
		State:     state,
		TargetUrl: target,
	})
}

// ListGithubApps implements GET /github-apps (permission: read).
func (a *API) ListGithubApps(w http.ResponseWriter, r *http.Request, params api.ListGithubAppsParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListGithubAppsPage(r.Context(), store.ListGithubAppsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list github apps", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(g store.GithubApp) int64 { return g.ID })
	data := make([]api.GithubApp, 0, len(rows))
	for _, row := range rows {
		data = append(data, githubAppToAPI(row))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": cursor})
}

func (a *API) resolveGithubApp(w http.ResponseWriter, r *http.Request, id *auth.Identity, appUUID string) (store.GithubApp, bool) {
	var u pgtype.UUID
	if err := u.Scan(appUUID); err == nil {
		row, err := a.Store.GetGithubAppByUUID(r.Context(), store.GetGithubAppByUUIDParams{Uuid: u, TeamID: id.TeamID})
		if err == nil {
			return row, true
		}
	}
	httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "github app not found")
	return store.GithubApp{}, false
}

// GetGithubApp implements GET /github-apps/{github_app_uuid}.
func (a *API) GetGithubApp(w http.ResponseWriter, r *http.Request, githubAppUuid api.GithubAppUuid) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveGithubApp(w, r, id, githubAppUuid)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, githubAppToAPI(row))
}

// DeleteGithubApp implements DELETE /github-apps/{github_app_uuid}.
func (a *API) DeleteGithubApp(w http.ResponseWriter, r *http.Request, githubAppUuid api.GithubAppUuid) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveGithubApp(w, r, id, githubAppUuid)
	if !ok {
		return
	}
	if _, err := a.Store.DeleteGithubApp(r.Context(), row.ID); err != nil {
		if isRestrictViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this GitHub App is still used by a git source — delete the applications that use it first")
			return
		}
		a.internalError(w, r, "delete github app", err)
		return
	}
	a.recordAudit(r, id, "github_app.delete", "github_app", row.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// githubClientFor builds the API client and token source of a converted app.
func (a *API) githubClientFor(row store.GithubApp) (*githubapp.Client, *githubapp.TokenSource, error) {
	if row.AppID == nil || row.AppPrivateKeyEnc == nil {
		return nil, nil, fmt.Errorf("the manifest flow is not finished for this app")
	}
	pem, err := a.Keyring.Decrypt("github_apps", "app_private_key_enc", uuidString(row.Uuid), row.AppPrivateKeyEnc)
	if err != nil {
		return nil, nil, err
	}
	client := &githubapp.Client{APIURL: row.ApiUrl}
	return client, githubapp.NewTokenSource(client, *row.AppID, pem), nil
}

// ListGithubAppRepositories implements GET /github-apps/{uuid}/repositories:
// the discovery cache, resynchronized from GitHub on demand (§2.1 step 8).
func (a *API) ListGithubAppRepositories(w http.ResponseWriter, r *http.Request, githubAppUuid api.GithubAppUuid, params api.ListGithubAppRepositoriesParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	row, ok := a.resolveGithubApp(w, r, id, githubAppUuid)
	if !ok {
		return
	}
	if row.InstallationID == nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "this GitHub App is not installed yet — install it on your account or organization first")
		return
	}
	source, err := a.Store.GetGitSourceForGithubApp(r.Context(), &row.ID)
	if err != nil {
		a.internalError(w, r, "github repositories", err)
		return
	}

	cached, err := a.Store.ListRepositoriesForSource(r.Context(), source.ID)
	if err != nil {
		a.internalError(w, r, "github repositories", err)
		return
	}
	if (params.Refresh != nil && *params.Refresh) || len(cached) == 0 {
		if err := a.syncGithubRepositories(r, row, source.ID); err != nil {
			a.internalError(w, r, "github repository discovery", err)
			return
		}
		if cached, err = a.Store.ListRepositoriesForSource(r.Context(), source.ID); err != nil {
			a.internalError(w, r, "github repositories", err)
			return
		}
	}

	data := make([]api.GitRepository, 0, len(cached))
	for _, repo := range cached {
		data = append(data, api.GitRepository{
			Uuid:          ptr(uuidString(repo.Uuid)),
			FullName:      ptr(repo.FullName),
			DefaultBranch: repo.DefaultBranch,
			HtmlUrl:       repo.HtmlUrl,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// syncGithubRepositories refreshes the discovery cache (§7.3).
func (a *API) syncGithubRepositories(r *http.Request, row store.GithubApp, sourceID int64) error {
	client, tokens, err := a.githubClientFor(row)
	if err != nil {
		return err
	}
	token, err := tokens.Token(r.Context(), *row.InstallationID, nil)
	if err != nil {
		return err
	}
	repos, err := client.ListInstallationRepos(r.Context(), token)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		ids = append(ids, fmt.Sprint(repo.ID))
		if _, err := a.Store.UpsertRepository(r.Context(), store.UpsertRepositoryParams{
			GitSourceID: sourceID, ExternalID: fmt.Sprint(repo.ID), FullName: repo.FullName,
			DefaultBranch: ptr(repo.DefaultBranch), HtmlUrl: ptr(repo.HTMLURL),
		}); err != nil {
			return err
		}
	}
	_, err = a.Store.DeleteVanishedRepositories(r.Context(), store.DeleteVanishedRepositoriesParams{
		GitSourceID: sourceID, ExternalIds: ids,
	})
	return err
}

// GithubManifestCallback handles GET /webhooks/github/manifest/callback —
// the browser redirect of §2.1 step 4/5. No bearer: the one-shot state IS
// the authentication.
func (a *API) GithubManifestCallback(w http.ResponseWriter, r *http.Request) {
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	stateHash := sha256.Sum256([]byte(state))
	hash := hex.EncodeToString(stateHash[:])
	draft, err := a.Store.GetGithubAppByStateHash(r.Context(), &hash)
	if err != nil {
		// Expired, replayed, or forged: one generic answer for all three.
		http.Error(w, "unknown or expired state", http.StatusNotFound)
		return
	}

	client := &githubapp.Client{APIURL: draft.ApiUrl}
	creds, err := client.ConvertManifest(r.Context(), code)
	if err != nil {
		a.Logger.Error("github manifest conversion failed", "github_app", uuidString(draft.Uuid), "error", err)
		http.Error(w, "the manifest conversion failed — restart the flow from the dashboard", http.StatusBadGateway)
		return
	}

	appUUID := uuidString(draft.Uuid)
	encrypt := func(column, value string) ([]byte, error) {
		return a.Keyring.Encrypt("github_apps", column, appUUID, []byte(value))
	}
	clientSecret, err1 := encrypt("client_secret_enc", creds.ClientSecret)
	webhookSecret, err2 := encrypt("webhook_secret_enc", creds.WebhookSecret)
	privateKey, err3 := encrypt("app_private_key_enc", creds.PEM)
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	row, err := a.Store.CompleteGithubAppConversion(r.Context(), store.CompleteGithubAppConversionParams{
		ID: draft.ID, Name: creds.Name, AppID: ptr(creds.AppID), Slug: ptr(creds.Slug),
		ClientID: ptr(creds.ClientID), ClientSecretEnc: clientSecret,
		WebhookSecretEnc: webhookSecret, AppPrivateKeyEnc: privateKey,
		HtmlUrl: creds.HTMLURL,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "unknown or expired state", http.StatusNotFound) // raced replay
			return
		}
		a.internalError(w, r, "github app conversion", err)
		return
	}
	// The git source is what applications reference (INV-002).
	if _, err := a.Store.CreateGithubAppSource(r.Context(), store.CreateGithubAppSourceParams{
		TeamID: row.TeamID, Name: row.Name, ApiUrl: ptr(row.ApiUrl), HtmlUrl: ptr(row.HtmlUrl),
		GithubAppID: &row.ID, CreatedBy: row.CreatedBy,
	}); err != nil && !isUniqueViolation(err) {
		a.internalError(w, r, "github app source", err)
		return
	}
	a.Logger.Info("github app converted", "github_app", appUUID, "app_id", creds.AppID)

	// Straight to the installation page: installing is the next step, and the
	// setup callback brings the browser back to the dashboard.
	if row.Slug != nil {
		http.Redirect(w, r, strings.TrimRight(row.HtmlUrl, "/")+"/apps/"+*row.Slug+"/installations/new", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/github-apps", http.StatusFound)
}

// GithubAppSetup handles GET /webhooks/github/apps/{app_uuid}/setup — the
// post-install browser redirect (§2.1 step 7). Redundant with the
// `installation` webhook: the first of the two wins.
func (a *API) GithubAppSetup(w http.ResponseWriter, r *http.Request) {
	var u pgtype.UUID
	if err := u.Scan(chi.URLParam(r, "app_uuid")); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	row, err := a.Store.GetGithubAppByUUIDAny(r.Context(), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if idParam := r.URL.Query().Get("installation_id"); idParam != "" && row.InstallationID == nil {
		var installationID int64
		if _, err := fmt.Sscanf(idParam, "%d", &installationID); err == nil && installationID > 0 {
			_, _ = a.Store.SetGithubAppInstallation(r.Context(), store.SetGithubAppInstallationParams{
				ID: row.ID, InstallationID: &installationID,
			})
			a.Logger.Info("github app installed (setup redirect)", "github_app", uuidString(row.Uuid), "installation_id", installationID)
		}
	}
	http.Redirect(w, r, "/github-apps", http.StatusFound)
}

// ReceiveGithubAppWebhook handles POST /webhooks/github/apps/{app_uuid} —
// the app-level webhook (§2.4/§2.5): one endpoint for every installed repo,
// authenticated by the app's HMAC secret over the raw body.
func (a *API) ReceiveGithubAppWebhook(w http.ResponseWriter, r *http.Request) {
	var u pgtype.UUID
	if err := u.Scan(chi.URLParam(r, "app_uuid")); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	row, err := a.Store.GetGithubAppByUUIDAny(r.Context(), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if row.WebhookSecretEnc == nil {
		// Draft app: no secret yet, nothing verifiable (§2.5).
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, gitwebhook.MaxBodyBytes+1))
	if err != nil || len(body) > int(gitwebhook.MaxBodyBytes) {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	secret, err := a.Keyring.Decrypt("github_apps", "webhook_secret_enc", uuidString(row.Uuid), row.WebhookSecretEnc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	deliveryID := gitwebhook.DeliveryID(gitwebhook.GitHub, r.Header)
	if deliveryID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	eventType := gitwebhook.EventType(gitwebhook.GitHub, r.Header)
	sigErr := gitwebhook.VerifySignature(gitwebhook.GitHub, r.Header, body, secret)

	status := store.WebhookDeliveryStatusReceived
	if sigErr != nil {
		status = store.WebhookDeliveryStatusFailed
	}
	delivery, err := a.Store.CreateWebhookDelivery(r.Context(), store.CreateWebhookDeliveryParams{
		Provider: store.WebhookProviderGithub, DeliveryID: deliveryID,
		EventType: &eventType, SignatureValid: sigErr == nil, Status: status,
		Payload: truncatedPayload(body), TeamID: ptr(row.TeamID),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
		return
	case err != nil:
		a.internalError(w, r, "receive github app webhook", err)
		return
	}
	if sigErr != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch eventType {
	case "installation":
		a.handleInstallationEvent(w, r, row, body, delivery.ID)
	case "installation_repositories":
		// Membership changed: drop the flag; the next discovery resyncs.
		_ = a.Store.FinishWebhookDelivery(r.Context(), store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusAccepted,
		})
		if source, err := a.Store.GetGitSourceForGithubApp(r.Context(), &row.ID); err == nil {
			_ = a.syncGithubRepositories(r, row, source.ID)
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
	case "push":
		var payload struct {
			Repository struct {
				ID int64 `json:"id"`
			} `json:"repository"`
		}
		_ = json.Unmarshal(body, &payload)
		if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
			Queue: "webhook",
			Type:  jobs.TypeGithubAppPush,
			Payload: jobs.GithubAppPushPayload{
				DeliveryID: delivery.ID, GithubAppID: row.ID, RepositoryID: payload.Repository.ID,
			},
			TeamID: ptr(row.TeamID),
		}); err != nil {
			a.internalError(w, r, "receive github app webhook", err)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
	case "pull_request":
		if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
			Queue: "webhook",
			Type:  jobs.TypeGithubAppPullRequest,
			Payload: jobs.GithubAppPullRequestPayload{
				DeliveryID: delivery.ID, GithubAppID: row.ID,
			},
			TeamID: ptr(row.TeamID),
		}); err != nil {
			a.internalError(w, r, "receive github app webhook", err)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
	default:
		// issue_comment (commands) arrives with the trigger controls;
		// recorded, visible, ignored with a reason — never silently dropped.
		reason := "event_not_handled"
		_ = a.Store.FinishWebhookDelivery(r.Context(), store.FinishWebhookDeliveryParams{
			ID: delivery.ID, Status: store.WebhookDeliveryStatusIgnored, IgnoreReason: &reason,
		})
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
	}
}

// handleInstallationEvent keeps installation_id in sync (§2.4).
func (a *API) handleInstallationEvent(w http.ResponseWriter, r *http.Request, row store.GithubApp, body []byte, deliveryID int64) {
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	_ = json.Unmarshal(body, &payload)
	switch payload.Action {
	case "created", "unsuspend", "new_permissions_accepted":
		if payload.Installation.ID > 0 {
			_, _ = a.Store.SetGithubAppInstallation(r.Context(), store.SetGithubAppInstallationParams{
				ID: row.ID, InstallationID: &payload.Installation.ID,
			})
		}
	case "deleted", "suspend":
		_, _ = a.Store.ClearGithubAppInstallation(r.Context(), row.ID)
		a.Logger.Warn("github app installation degraded", "github_app", uuidString(row.Uuid), "action", payload.Action)
	}
	_ = a.Store.FinishWebhookDelivery(r.Context(), store.FinishWebhookDeliveryParams{
		ID: deliveryID, Status: store.WebhookDeliveryStatusAccepted,
	})
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
}


// isRestrictViolation matches the RESTRICT foreign-key SQLSTATE (23503):
// the app is still referenced by a git source.
func isRestrictViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
