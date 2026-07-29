package handlers

import (
	"encoding/json"
	"net/http"
	"net/netip"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

func tokenToAPI(t store.ApiToken) api.ApiToken {
	perms := make([]api.ApiTokenPermission, 0, len(t.Permissions))
	for _, p := range t.Permissions {
		perms = append(perms, api.ApiTokenPermission(p))
	}
	allowlist := make([]string, 0, len(t.IpAllowlist))
	for _, p := range t.IpAllowlist {
		allowlist = append(allowlist, p.String())
	}
	return api.ApiToken{
		Uuid:        ptr(uuidString(t.Uuid)),
		Name:        t.Name,
		Permissions: perms,
		TokenPrefix: ptr(t.TokenPrefix),
		IpAllowlist: &allowlist,
		ExpiresAt:   timePtr(t.ExpiresAt),
		LastUsedAt:  timePtr(t.LastUsedAt),
		CreatedAt:   timePtr(t.CreatedAt),
	}
}

// ListApiTokens implements GET /teams/{team_uuid}/tokens (permission:
// read). Metadata only — the token value is never returned (§10.3).
func (a *API) ListApiTokens(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.ListApiTokensParams) {
	id, ok := a.require(w, r, auth.PermTokensRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
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

	rows, err := a.Store.ListApiTokensPage(r.Context(), store.ListApiTokensPageParams{
		TeamID: team.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list api tokens", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(t store.ApiToken) int64 { return t.ID })

	data := make([]api.ApiToken, 0, len(rows))
	for _, t := range rows {
		data = append(data, tokenToAPI(t))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.ApiToken `json:"data"`
		NextCursor *string        `json:"next_cursor"`
	}{data, cursor})
}

// CreateApiToken implements POST /teams/{team_uuid}/tokens (permission:
// write). Anti privilege escalation: a token cannot grant a permission it
// does not hold itself (§10.3).
func (a *API) CreateApiToken(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.CreateApiTokenParams) {
	id, ok := a.require(w, r, auth.PermTokensCreate)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}

	var body api.ApiTokenCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	var details []api.ErrorDetail
	if body.Name == "" || len(body.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	if len(body.Permissions) == 0 {
		details = append(details, api.ErrorDetail{Field: ptr("permissions"), Code: ptr("required"), Message: "at least one permission is required"})
	}
	perms := make([]string, 0, len(body.Permissions))
	for _, p := range body.Permissions {
		if !validPermission(string(p)) {
			details = append(details, api.ErrorDetail{Field: ptr("permissions"), Code: ptr("out_of_range"), Message: "unknown permission " + string(p)})
			continue
		}
		perms = append(perms, string(p))
	}
	var allowlist []netip.Prefix
	if body.IpAllowlist != nil {
		for _, cidr := range *body.IpAllowlist {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				details = append(details, api.ErrorDetail{Field: ptr("ip_allowlist"), Code: ptr("invalid_cidr"), Message: "invalid CIDR " + cidr})
				continue
			}
			allowlist = append(allowlist, prefix)
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	for _, p := range perms {
		if !auth.Has(id.Permissions, auth.Permission(p)) {
			httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
				"a token cannot grant the "+p+" permission it does not hold itself")
			return
		}
	}

	value, prefix, hash, err := auth.NewToken()
	if err != nil {
		a.internalError(w, r, "generate token", err)
		return
	}
	var expiresAt pgtype.Timestamptz
	if body.ExpiresAt != nil {
		expiresAt = pgtype.Timestamptz{Time: body.ExpiresAt.UTC(), Valid: true}
	}

	created, err := a.Store.CreateApiToken(r.Context(), store.CreateApiTokenParams{
		TeamID:      team.ID,
		Name:        body.Name,
		TokenPrefix: prefix,
		TokenHash:   hash,
		Permissions: perms,
		IpAllowlist: allowlist,
		ExpiresAt:   expiresAt,
		// Who minted it: the token can never outgrow this person's own
		// permissions (rbac-matrix §4.2).
		CreatedBy: actingUserID(id),
	})
	if err != nil {
		a.internalError(w, r, "create api token", err)
		return
	}

	a.recordAudit(r, id, "token.create", "api_token", created.Uuid)
	a.Logger.Info("api token created", "team_uuid", uuidString(team.Uuid), "token_uuid", uuidString(created.Uuid))

	meta := tokenToAPI(created)
	httpapi.WriteJSON(w, http.StatusCreated, api.ApiTokenCreated{
		Uuid:        meta.Uuid,
		Name:        meta.Name,
		Permissions: meta.Permissions,
		TokenPrefix: meta.TokenPrefix,
		IpAllowlist: meta.IpAllowlist,
		ExpiresAt:   meta.ExpiresAt,
		LastUsedAt:  meta.LastUsedAt,
		CreatedAt:   meta.CreatedAt,
		Token:       value, // clear value — present only in this response
	})
}

// RevokeApiToken implements DELETE /teams/{team_uuid}/tokens/{token_uuid}
// (permission: write). Immediate and final; a token may revoke itself.
func (a *API) RevokeApiToken(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, tokenUuid api.TokenUuid) {
	id, ok := a.require(w, r, auth.PermTokensRevoke)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(tokenUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "token not found")
		return
	}
	rows, err := a.Store.RevokeApiTokenByUUID(r.Context(), store.RevokeApiTokenByUUIDParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		a.internalError(w, r, "revoke api token", err)
		return
	}
	if rows == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "token not found")
		return
	}
	a.recordAudit(r, id, "token.revoke", "api_token", u)
	a.Logger.Info("api token revoked", "team_uuid", uuidString(team.Uuid), "token_uuid", tokenUuid, "by_token_uuid", id.TokenUUID)
	w.WriteHeader(http.StatusNoContent)
}

func validPermission(p string) bool {
	for _, known := range auth.AllPermissions {
		if p == string(known) {
			return true
		}
	}
	return false
}
