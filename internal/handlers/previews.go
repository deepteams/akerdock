// PR previews of an application (§20.4): listing and the explicit fork
// approval of §20.4.8 — the one gate a fork PR must pass before anything of
// it is built (INV-010).
package handlers

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
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
