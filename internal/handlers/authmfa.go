package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// TOTP 2FA endpoints (PRD §10.2, §23.3). Like the rest of /auth they live
// outside /api/v1: the v1 contract authenticates with a Bearer token and a
// bearer token never faces a second factor — its permissions and IP allowlist
// are its controls (§10.3).
//
// Two groups with opposite authentication:
//
//   - /auth/mfa/verify finishes a two-step LOGIN: no session yet, the
//     challenge token from /auth/login is the only credential.
//   - /auth/mfa/totp/* and /auth/mfa manage the factor for a signed-in user:
//     session + CSRF, like the passkey management endpoints.

// mfaCodeBody is what every code-carrying endpoint posts: a TOTP code, or a
// recovery code where accepted.
type mfaCodeBody struct {
	Challenge    string `json:"challenge,omitempty"`
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

// readMFABody decodes the small JSON bodies of the MFA endpoints.
func readMFABody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(into); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// sessionIdentity adapts an authenticated session row into the audit
// identity: the /auth routes sit outside the bearer middleware, so nothing
// put one in the context for us.
func sessionIdentity(sess *store.GetSessionByTokenHashRow) *auth.Identity {
	teamID := int64(0)
	if sess.CurrentTeamID != nil {
		teamID = *sess.CurrentTeamID
	}
	return &auth.Identity{TokenUUID: uuidString(sess.Uuid), TeamID: teamID, Session: true}
}

// VerifyMFALogin implements POST /auth/mfa/verify: step two of a login that
// answered mfa_required. Same response shape as /auth/login — to the
// dashboard, a finished two-step login IS a login.
func (a *API) VerifyMFALogin(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	var body mfaCodeBody
	if !readMFABody(w, r, &body) {
		return
	}

	sess, token, err := a.MFA.VerifyLogin(r.Context(), r, body.Challenge, body.Code, body.RecoveryCode)
	switch {
	case errors.Is(err, session.ErrMFAChallengeExpired):
		a.auditAuth(r, "auth.mfa", store.AuditResultFailure, 0, "", nil)
		httpapi.WriteError(w, r, http.StatusBadRequest, "challenge_expired", err.Error())
		return
	case errors.Is(err, session.ErrAccountLocked):
		a.auditAuth(r, "auth.mfa", store.AuditResultDenied, 0, "", nil)
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "account_locked",
			"too many failed attempts — try again later")
		return
	case errors.Is(err, session.ErrMFACodeInvalid):
		// One answer for a wrong code, a replayed code and a spent recovery
		// code: anything finer grades an attacker's guesses for them.
		a.Logger.Warn("failed MFA verification", "ip", r.RemoteAddr)
		a.auditAuth(r, "auth.mfa", store.AuditResultFailure, 0, "", nil)
		httpapi.WriteError(w, r, http.StatusUnauthorized, "invalid_code", err.Error())
		return
	case err != nil:
		a.internalError(w, r, "mfa verify", err)
		return
	}

	if a.refuseUndeliverableSession(w, r) {
		return
	}
	a.Sessions.SetCookies(w, token, sess.CSRFToken)
	a.Logger.Info("session opened after MFA", "user", sess.Email, "team_id", sess.TeamID)
	mfaTeamID := sess.TeamID
	a.auditAuth(r, "auth.mfa", store.AuditResultSuccess, sess.UserID, sess.Email, &mfaTeamID)

	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"email":      sess.Email,
		"name":       sess.Name,
		"role":       string(sess.Role),
		"csrf_token": sess.CSRFToken,
		"expires_at": time.Now().Add(session.Lifetime).UTC(),
	})
}

// MFAStatus implements GET /auth/mfa: what the security settings page shows.
func (a *API) MFAStatus(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	enabled, confirmedAt, left, err := a.MFA.Enabled(r.Context(), sess.UserID)
	if err != nil {
		a.internalError(w, r, "mfa status", err)
		return
	}
	out := map[string]any{"enabled": enabled, "recovery_codes_remaining": left}
	if enabled {
		out["confirmed_at"] = confirmedAt.UTC()
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// SetupMFATOTP implements POST /auth/mfa/totp/setup: mints the secret the
// authenticator app will hold. The factor guards nothing until confirmed —
// this response is the ONLY time the secret travels in clear, and it goes to
// an authenticated, CSRF-checked session.
func (a *API) SetupMFATOTP(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	secret, uri, err := a.MFA.Setup(r.Context(), sess.UserID)
	switch {
	case errors.Is(err, session.ErrMFAAlreadyEnabled):
		httpapi.WriteError(w, r, http.StatusConflict, "mfa_already_enabled",
			"two-factor authentication is already enabled — disable it first")
		return
	case err != nil:
		a.internalError(w, r, "mfa setup", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"secret":      secret,
		"otpauth_uri": uri,
	})
}

// ConfirmMFATOTP implements POST /auth/mfa/totp/confirm: the first valid
// code turns the factor on. The recovery codes in the response exist nowhere
// else and never will again — the UI's job is to make the user keep them.
func (a *API) ConfirmMFATOTP(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body mfaCodeBody
	if !readMFABody(w, r, &body) {
		return
	}

	codes, err := a.MFA.Confirm(r.Context(), sess.UserID, body.Code)
	switch {
	case errors.Is(err, session.ErrMFANotConfigured):
		httpapi.WriteError(w, r, http.StatusConflict, "mfa_not_configured",
			"no pending setup — call setup first")
		return
	case errors.Is(err, session.ErrMFAAlreadyEnabled):
		httpapi.WriteError(w, r, http.StatusConflict, "mfa_already_enabled",
			"two-factor authentication is already enabled")
		return
	case errors.Is(err, session.ErrMFACodeInvalid):
		a.Logger.Warn("failed MFA confirmation", "user", sess.Email, "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusBadRequest, "invalid_code",
			"the code does not match — scan the QR code again and retry")
		return
	case err != nil:
		a.internalError(w, r, "mfa confirm", err)
		return
	}

	a.recordAudit(r, sessionIdentity(sess), "mfa.enable", "user", userUUIDOf(a, r, sess.UserID))
	a.Logger.Info("MFA TOTP enabled", "user", sess.Email)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// DisableMFATOTP implements DELETE /auth/mfa/totp. Turning 2FA off demands a
// currently-valid code or a recovery code: a hijacked session must not strip
// the account's second factor with one click. Audited (§23.4,
// data-dictionary §4.3: « désactivation auditée »).
func (a *API) DisableMFATOTP(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body mfaCodeBody
	if !readMFABody(w, r, &body) {
		return
	}

	err := a.MFA.Disable(r.Context(), sess.UserID, body.Code, body.RecoveryCode)
	switch {
	case errors.Is(err, session.ErrMFANotConfigured):
		httpapi.WriteError(w, r, http.StatusNotFound, "mfa_not_configured",
			"two-factor authentication is not enabled")
		return
	case errors.Is(err, session.ErrAccountLocked):
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "account_locked",
			"too many failed attempts — try again later")
		return
	case errors.Is(err, session.ErrMFACodeInvalid):
		a.Audit.Record(r, sessionIdentity(sess), audit.Event{
			Action: "mfa.disable", TargetKind: "user",
			TargetUUID: userUUIDOf(a, r, sess.UserID), Result: store.AuditResultDenied,
		})
		a.Logger.Warn("failed MFA disable attempt", "user", sess.Email, "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusBadRequest, "invalid_code", "invalid code")
		return
	case err != nil:
		a.internalError(w, r, "mfa disable", err)
		return
	}

	a.recordAudit(r, sessionIdentity(sess), "mfa.disable", "user", userUUIDOf(a, r, sess.UserID))
	a.Logger.Info("MFA TOTP disabled", "user", sess.Email)
	w.WriteHeader(http.StatusNoContent)
}

// RegenerateMFARecoveryCodes implements POST /auth/mfa/recovery-codes: a new
// set against a valid TOTP code, replacing whatever remained of the old one.
func (a *API) RegenerateMFARecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if a.MFA == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body mfaCodeBody
	if !readMFABody(w, r, &body) {
		return
	}

	codes, err := a.MFA.RegenerateRecoveryCodes(r.Context(), sess.UserID, body.Code)
	switch {
	case errors.Is(err, session.ErrMFANotConfigured):
		httpapi.WriteError(w, r, http.StatusNotFound, "mfa_not_configured",
			"two-factor authentication is not enabled")
		return
	case errors.Is(err, session.ErrAccountLocked):
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "account_locked",
			"too many failed attempts — try again later")
		return
	case errors.Is(err, session.ErrMFACodeInvalid):
		a.Logger.Warn("failed recovery-code regeneration", "user", sess.Email, "ip", r.RemoteAddr)
		httpapi.WriteError(w, r, http.StatusBadRequest, "invalid_code", "invalid code")
		return
	case err != nil:
		a.internalError(w, r, "mfa recovery codes", err)
		return
	}

	a.recordAudit(r, sessionIdentity(sess), "mfa.recovery_codes.regenerate", "user", userUUIDOf(a, r, sess.UserID))
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// userUUIDOf resolves a user's public uuid for the audit target. Best-effort:
// an audit row with a null target still beats no audit row.
func userUUIDOf(a *API, r *http.Request, userID int64) pgtype.UUID {
	user, err := a.Store.GetUserByID(r.Context(), userID)
	if err != nil {
		return pgtype.UUID{}
	}
	return user.Uuid
}
