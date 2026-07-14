package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// Passkey (WebAuthn) endpoints. Like /auth/login they live outside /api/v1:
// they exist for the dashboard alone, and the v1 contract knows nothing of
// sessions (§10.2).
//
// The ceremony protocol is two half-requests: begin hands the browser the
// library's options plus an opaque ceremony token; finish echoes the token
// with the authenticator's response. The token is single-use and expires in
// minutes — see session.Passkeys.

// passkeyName bounds the human label of a credential. Purely cosmetic data,
// but stored and displayed: bound it before it becomes a stored-XSS vector or
// a megabyte of nothing.
func passkeyName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "passkey"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// sessionUser authenticates the request by session cookie and, for mutations,
// verifies the CSRF echo. Passkey management is exactly the kind of endpoint
// CSRF was invented for: "add your key to my account" must not be one hidden
// form away.
func (a *API) sessionUser(w http.ResponseWriter, r *http.Request) (*store.GetSessionByTokenHashRow, bool) {
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return nil, false
	}
	if err := a.Sessions.VerifyCSRF(r.Context(), r); err != nil {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "missing or invalid CSRF token")
		return nil, false
	}
	return sess, true
}

// BeginPasskeyRegistration implements POST /auth/passkeys/register/begin.
func (a *API) BeginPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	options, ceremony, err := a.Passkeys.BeginRegistration(r.Context(), sess.UserID)
	if err != nil {
		a.internalError(w, r, "passkey registration begin", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ceremony": ceremony, "options": options})
}

// FinishPasskeyRegistration implements POST /auth/passkeys/register/finish.
func (a *API) FinishPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Ceremony   string          `json:"ceremony"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	cred, err := a.Passkeys.FinishRegistration(r.Context(), sess.UserID, body.Ceremony, passkeyName(body.Name), body.Credential)
	switch {
	case errors.Is(err, session.ErrCeremonyExpired):
		httpapi.WriteError(w, r, http.StatusBadRequest, "ceremony_expired", err.Error())
		return
	case errors.Is(err, session.ErrPasskeyRejected):
		a.Logger.Warn("passkey registration rejected", "user", sess.Email, "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusBadRequest, "passkey_rejected", err.Error())
		return
	case isUniqueViolation(err):
		httpapi.WriteError(w, r, http.StatusConflict, "passkey_exists", "this authenticator is already registered")
		return
	case err != nil:
		a.internalError(w, r, "passkey registration finish", err)
		return
	}

	a.Logger.Info("passkey enrolled", "user", sess.Email, "passkey", cred.Name)
	httpapi.WriteJSON(w, http.StatusCreated, passkeyJSON(cred))
}

// ListPasskeys implements GET /auth/passkeys.
func (a *API) ListPasskeys(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	rows, err := a.Store.ListPasskeysForUser(r.Context(), sess.UserID)
	if err != nil {
		a.internalError(w, r, "passkey list", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, passkeyJSON(row))
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// DeletePasskey implements DELETE /auth/passkeys/{passkey_uuid}.
func (a *API) DeletePasskey(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(chi.URLParam(r, "passkey_uuid")); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "passkey not found")
		return
	}
	n, err := a.Store.DeletePasskeyForUser(r.Context(), store.DeletePasskeyForUserParams{
		Uuid: u, UserID: sess.UserID,
	})
	if err != nil {
		a.internalError(w, r, "passkey delete", err)
		return
	}
	if n == 0 {
		// Someone else's passkey and a missing one answer the same: the row
		// count is scoped by user in SQL, so there is nothing to leak.
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "passkey not found")
		return
	}
	a.Logger.Info("passkey revoked", "user", sess.Email)
	w.WriteHeader(http.StatusNoContent)
}

// BeginPasskeyLogin implements POST /auth/passkey/login/begin. Anonymous by
// design — the whole point of a discoverable login is that the server does
// not know who is coming.
func (a *API) BeginPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	options, ceremony, err := a.Passkeys.BeginLogin(r.Context())
	if err != nil {
		a.internalError(w, r, "passkey login begin", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ceremony": ceremony, "options": options})
}

// FinishPasskeyLogin implements POST /auth/passkey/login/finish: the passkey
// counterpart of Login, with the same session-minting tail.
func (a *API) FinishPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if a.Passkeys == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	var body struct {
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	sess, token, err := a.Passkeys.FinishLogin(r.Context(), r, body.Ceremony, body.Credential)
	switch {
	case errors.Is(err, session.ErrCeremonyExpired):
		httpapi.WriteError(w, r, http.StatusBadRequest, "ceremony_expired", err.Error())
		return
	case errors.Is(err, session.ErrPasskeyClone):
		// This one is loud on purpose: a rewound signature counter means the
		// credential exists twice, and the owner must revoke it.
		a.Logger.Error("passkey clone detected", "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusUnauthorized, "passkey_clone_detected", err.Error())
		return
	case errors.Is(err, session.ErrPasskeyRejected):
		a.Logger.Warn("failed passkey login attempt", "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "passkey verification failed")
		return
	case err != nil:
		a.internalError(w, r, "passkey login finish", err)
		return
	}

	a.Sessions.SetCookies(w, token, sess.CSRFToken)
	a.Logger.Info("session opened by passkey", "user", sess.Email, "team_id", sess.TeamID)

	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"email":      sess.Email,
		"name":       sess.Name,
		"role":       string(sess.Role),
		"csrf_token": sess.CSRFToken,
		"expires_at": time.Now().Add(session.Lifetime).UTC(),
	})
}

// passkeyJSON is the credential as the dashboard sees it: the label and the
// timestamps, never the key material — the UI manages passkeys, it does not
// transport them.
func passkeyJSON(p store.PasskeyCredential) map[string]any {
	out := map[string]any{
		"uuid":       uuidString(p.Uuid),
		"name":       p.Name,
		"created_at": p.CreatedAt.Time.UTC(),
	}
	if p.LastUsedAt.Valid {
		out["last_used_at"] = p.LastUsedAt.Time.UTC()
	} else {
		out["last_used_at"] = nil
	}
	return out
}
