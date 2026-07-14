package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/oidc"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// OAuth/OIDC provider configuration (§10.2, amendement n°30). Root-only,
// like the rest of /system: whoever holds these credentials decides who can
// sign in. The client secret follows the secret store (§23.2) — written,
// encrypted, never read back.

// ListOauthProviders implements GET /system/oauth-providers.
func (a *API) ListOauthProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.require(w, r, auth.PermRoot); !ok {
		return
	}
	rows, err := a.Store.ListOauthProviderConfigs(r.Context())
	if err != nil {
		a.internalError(w, r, "oauth providers list", err)
		return
	}
	out := make([]api.OauthProviderConfig, 0, len(rows))
	for _, row := range rows {
		out = append(out, oauthProviderToAPI(row))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// SetOauthProvider implements PUT /system/oauth-providers/{oauth_provider}.
func (a *API) SetOauthProvider(w http.ResponseWriter, r *http.Request, oauthProvider api.SetOauthProviderParamsOauthProvider) {
	id, ok := a.require(w, r, auth.PermRoot)
	if !ok {
		return
	}
	var body api.OauthProviderSet
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	provider := string(oauthProvider)
	profile := oidc.Profiles[provider] // the enum bounds the value; the profile always exists
	if body.ClientId == "" || body.ClientSecret == "" {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("client_id"), Code: ptr("required"),
			Message: "client_id and client_secret are required",
		}})
		return
	}

	// The issuer is exactly as optional as the provider family says: required
	// where discovery is the point (oidc, azure), refused where the endpoints
	// are pinned — a configurable endpoint on a fixed brand would let a typo
	// send the client secret elsewhere.
	issuer := ""
	if body.IssuerUrl != nil {
		issuer = strings.TrimSpace(*body.IssuerUrl)
	}
	switch {
	case profile.NeedsIssuer && issuer == "":
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("issuer_url"), Code: ptr("required"),
			Message: "this provider needs its OpenID Connect issuer URL",
		}})
		return
	case !profile.NeedsIssuer && issuer != "":
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("issuer_url"), Code: ptr("invalid"),
			Message: "this provider's endpoints are fixed — issuer_url is not accepted",
		}})
		return
	case issuer != "":
		if err := oidc.ValidateIssuer(issuer); err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("issuer_url"), Code: ptr("invalid"), Message: err.Error(),
			}})
			return
		}
	}

	// The row uuid is minted per write and carried into the envelope AAD: a
	// re-configured secret gets a fresh binding, and an old ciphertext cannot
	// be replayed onto the new row.
	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "oauth provider set", err)
		return
	}
	enc, err := a.Keyring.Encrypt("oauth_provider_configs", "client_secret_enc", pguuid.String(u), []byte(body.ClientSecret))
	if err != nil {
		a.internalError(w, r, "oauth provider set", err)
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	row, err := a.Store.UpsertOauthProviderConfig(r.Context(), store.UpsertOauthProviderConfigParams{
		Uuid:            u,
		Provider:        store.OauthProvider(provider),
		DisplayName:     body.DisplayName,
		ClientID:        body.ClientId,
		ClientSecretEnc: enc,
		IssuerUrl:       nilIfEmpty(issuer),
		Enabled:         enabled,
	})
	if err != nil {
		a.internalError(w, r, "oauth provider set", err)
		return
	}

	// Audited with a REDACTED diff (§23.4): the audit trail records that the
	// secret changed, never what it became.
	a.recordAuditDiff(r, id, "system.oauth_provider.set", "oauth_provider", row.Uuid,
		nil, map[string]any{"provider": provider, "client_id": body.ClientId, "enabled": enabled, "issuer_url": issuer})
	httpapi.WriteJSON(w, http.StatusOK, oauthProviderToAPI(row))
}

// DeleteOauthProvider implements DELETE /system/oauth-providers/{oauth_provider}.
func (a *API) DeleteOauthProvider(w http.ResponseWriter, r *http.Request, oauthProvider api.DeleteOauthProviderParamsOauthProvider) {
	id, ok := a.require(w, r, auth.PermRoot)
	if !ok {
		return
	}
	n, err := a.Store.DeleteOauthProviderConfig(r.Context(), store.OauthProvider(oauthProvider))
	if err != nil {
		a.internalError(w, r, "oauth provider delete", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "provider not configured")
		return
	}
	a.recordAudit(r, id, "system.oauth_provider.delete", "oauth_provider", pguuid.MustParse(""))
	w.WriteHeader(http.StatusNoContent)
}

func oauthProviderToAPI(row store.OauthProviderConfig) api.OauthProviderConfig {
	out := api.OauthProviderConfig{
		Provider:    api.OauthProviderConfigProvider(row.Provider),
		Enabled:     row.Enabled,
		ClientId:    row.ClientID,
		DisplayName: row.DisplayName,
		IssuerUrl:   row.IssuerUrl,
	}
	if row.UpdatedAt.Valid {
		t := row.UpdatedAt.Time
		out.UpdatedAt = &t
	}
	return out
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
