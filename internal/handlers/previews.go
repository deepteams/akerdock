// PR previews of an application (§20.4): listing and the explicit fork
// approval of §20.4.8 — the one gate a fork PR must pass before anything of
// it is built (INV-010).
package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
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
			_, _, _ = jobs.TryPromotePreview(r.Context(), a.Store, a.Logger, appRow, refreshed)
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
