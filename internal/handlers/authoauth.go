package handlers

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// OAuth/OIDC login endpoints (§10.2). Outside /api/v1 like the rest of
// /auth: the round-trip is a browser affair, and the v1 contract knows
// nothing of sessions.
//
// The callback is the one endpoint here that ANSWERS WITH A REDIRECT, not
// JSON: the browser arrives on a top-level navigation from the identity
// provider, and the only useful thing to do with it is to send it back into
// the app — signed in, or on the sign-in page with an error code it can
// explain.

// OauthProviders implements GET /auth/oauth/providers: what the sign-in
// page draws its buttons from. Anonymous and secret-free by construction.
func (a *API) OauthProviders(w http.ResponseWriter, r *http.Request) {
	if a.OAuth == nil {
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	providers, err := a.OAuth.EnabledProviders(r.Context())
	if err != nil {
		a.internalError(w, r, "oauth providers", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": providers})
}

// StartOauth implements POST /auth/oauth/{oauth_provider}/start: mints the
// state and answers the URL the page navigates to. A POST answering JSON
// rather than a redirecting GET, for two reasons: a GET that changes state
// (it writes a row) invites prefetchers to start logins, and the 'link'
// purpose needs the CSRF check only mutations get.
func (a *API) StartOauth(w http.ResponseWriter, r *http.Request) {
	if a.OAuth == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	provider := chi.URLParam(r, "oauth_provider")

	purpose := "login"
	var userID *int64
	if r.URL.Query().Get("purpose") == "link" {
		// Linking attaches a federated identity to the SIGNED-IN account:
		// session + CSRF, exactly like enrolling a passkey — "add a way into
		// my account" must never be one hidden form away.
		sess, ok := a.sessionUser(w, r)
		if !ok {
			return
		}
		purpose, userID = "link", &sess.UserID
	}

	authorizeURL, err := a.OAuth.Start(r.Context(), provider, purpose, userID)
	switch {
	case errors.Is(err, session.ErrOAuthProviderUnavailable):
		httpapi.WriteError(w, r, http.StatusNotFound, "provider_unavailable", err.Error())
		return
	case err != nil:
		a.internalError(w, r, "oauth start", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"url": authorizeURL})
}

// OauthCallback implements GET /auth/oauth/{oauth_provider}/callback — where
// the identity provider sends the browser back.
func (a *API) OauthCallback(w http.ResponseWriter, r *http.Request) {
	if a.OAuth == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	provider := chi.URLParam(r, "oauth_provider")
	q := r.URL.Query()

	// The provider may come back with an error instead of a code (user
	// cancelled, consent denied). Not our failure — back to sign-in, labeled.
	if e := q.Get("error"); e != "" {
		a.Logger.Warn("oauth callback returned an error", "provider", provider, "error", e)
		a.auditAuth(r, "auth.oauth", store.AuditResultFailure, 0, provider, nil)
		redirectWithError(w, r, "/sign-in", "provider_refused")
		return
	}

	result, err := a.OAuth.Callback(r.Context(), r, provider, q.Get("state"), q.Get("code"))
	if err != nil {
		code := "oauth_failed"
		switch {
		case errors.Is(err, session.ErrOAuthStateInvalid):
			code = "state_invalid"
		case errors.Is(err, session.ErrOAuthAccountCollision):
			code = "account_exists"
		case errors.Is(err, session.ErrOAuthRegistrationDisabled):
			code = "registration_disabled"
		case errors.Is(err, session.ErrOAuthEmailMissing):
			code = "email_unverified"
		case errors.Is(err, session.ErrOAuthIdentityTaken):
			code = "identity_taken"
		}
		a.Logger.Warn("oauth callback failed", "provider", provider, "code", code, "error", err, "ip", r.RemoteAddr)
		a.auditAuth(r, "auth.oauth", store.AuditResultFailure, 0, provider, nil)
		target := "/sign-in"
		if code == "identity_taken" {
			target = "/security" // a link attempt: the user is signed in
		}
		redirectWithError(w, r, target, code)
		return
	}

	if result.Purpose == "link" {
		// The linker is signed in (the state row proved it); the audit entry
		// names the session as actor like every /auth mutation.
		if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
			a.recordAudit(r, sessionIdentity(sess), "identity.link", "user", userUUIDOf(a, r, sess.UserID))
		}
		a.Logger.Info("identity linked", "provider", provider)
		http.Redirect(w, r, "/security?linked="+url.QueryEscape(provider), http.StatusSeeOther)
		return
	}

	// A browser navigation, not an XHR: the refusal must land on the sign-in
	// page as a readable error, not as a JSON body nobody renders.
	if a.Sessions.CookiesWouldBeDropped(r) {
		a.Logger.Warn("oauth login refused: Secure session cookie would be dropped over plain HTTP",
			"provider", provider, "host", r.Host)
		http.Redirect(w, r, "/sign-in?error=https_required", http.StatusSeeOther)
		return
	}
	a.Sessions.SetCookies(w, result.SessionToken, result.Session.CSRFToken)
	a.Logger.Info("session opened by oauth", "provider", provider, "user", result.Session.Email, "team_id", result.Session.TeamID)
	oauthTeamID := result.Session.TeamID
	a.auditAuth(r, "auth.oauth", store.AuditResultSuccess, result.Session.UserID, result.Session.Email, &oauthTeamID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ListIdentities implements GET /auth/identities: the federated identities
// linked to the signed-in account, for the security page.
func (a *API) ListIdentities(w http.ResponseWriter, r *http.Request) {
	if a.OAuth == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	rows, err := a.Store.ListIdentitiesForUser(r.Context(), sess.UserID)
	if err != nil {
		a.internalError(w, r, "identities list", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"uuid":     uuidString(row.Uuid),
			"provider": string(row.Provider),
			"email":    row.Email,
		}
		if row.CreatedAt.Valid {
			item["created_at"] = row.CreatedAt.Time.UTC()
		}
		out = append(out, item)
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// DeleteIdentity implements DELETE /auth/identities/{identity_uuid}:
// explicit unlinking, audited, and refused when it would leave the account
// with no way in.
func (a *API) DeleteIdentity(w http.ResponseWriter, r *http.Request) {
	if a.OAuth == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(chi.URLParam(r, "identity_uuid")); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "identity not found")
		return
	}

	err := a.OAuth.Unlink(r.Context(), sess.UserID, u)
	switch {
	case errors.Is(err, session.ErrLastCredential):
		httpapi.WriteError(w, r, http.StatusConflict, "last_credential", err.Error())
		return
	case err != nil:
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "identity not found")
		return
	}

	a.recordAudit(r, sessionIdentity(sess), "identity.unlink", "user", userUUIDOf(a, r, sess.UserID))
	a.Logger.Info("identity unlinked", "user", sess.Email)
	w.WriteHeader(http.StatusNoContent)
}

// redirectWithError sends the browser back into the UI with a machine
// error code the page translates — never raw provider output, which is
// unbounded and unescaped.
func redirectWithError(w http.ResponseWriter, r *http.Request, path, code string) {
	http.Redirect(w, r, path+"?error="+url.QueryEscape(code), http.StatusSeeOther)
}
