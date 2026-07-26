package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// Browser authentication lives OUTSIDE /api/v1, like the Git webhooks: the v1
// contract authenticates with a Bearer token and says nothing about sessions
// (§10.2 puts login and MFA out of the API). Putting these under /api/v1 would
// have meant amending the contract to describe endpoints that exist for the UI
// alone, and that no API client should ever call.
//
// The dashboard is a first-class client of the API — it just gets in through a
// different door.

// loginRequest is what the sign-in form posts.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// refuseUndeliverableSession refuses to open a session whose cookie the
// browser would silently drop: Secure cookies (implied by a configured FQDN)
// over plain HTTP produce a 200 login followed by nothing but 401s, with no
// trace of why. Failing HERE, with the reason, is the only honest answer.
// Returns true when the request was refused.
func (a *API) refuseUndeliverableSession(w http.ResponseWriter, r *http.Request) bool {
	if a.Sessions == nil || !a.Sessions.CookiesWouldBeDropped(r) {
		return false
	}
	a.Logger.Warn("login refused: Secure session cookie would be dropped over plain HTTP",
		"host", r.Host, "ip", r.RemoteAddr)
	httpapi.WriteError(w, r, http.StatusBadRequest, "https_required",
		"this instance has a FQDN configured, so its session cookie is marked Secure — "+
			"but this request came over plain HTTP and the browser would never send the cookie back. "+
			"Serve the dashboard over HTTPS (or clear the instance FQDN to run plain HTTP).")
	return true
}

// Login implements POST /auth/login.
//
// Rate limiting matters here more than anywhere: this is the only endpoint that
// turns a guess into an answer. The account lockout in the session manager is
// the second line; the first is the per-IP limiter this handler sits behind.
func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	if a.refuseUndeliverableSession(w, r) {
		return
	}
	var body loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}

	sess, token, err := a.Sessions.Login(r.Context(), r, body.Email, body.Password)
	switch {
	case errors.Is(err, session.ErrMFARequired):
		// The password was right; the session does not exist yet. The token
		// here is the CHALLENGE the client echoes to /auth/mfa/verify with
		// its TOTP code — it names nobody and opens nothing on its own.
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"mfa_required": true,
			"challenge":    token,
		})
		return
	case errors.Is(err, session.ErrAccountLocked):
		// Told plainly: a locked-out user needs to know why they cannot get in,
		// and an attacker learns nothing they could not measure anyway.
		a.auditAuth(r, "auth.login", store.AuditResultDenied, 0, body.Email, nil)
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "account_locked",
			"too many failed attempts — try again later")
		return
	case errors.Is(err, session.ErrInvalidCredentials):
		// One message for a wrong email AND a wrong password: anything else
		// turns this endpoint into an account-enumeration oracle.
		a.Logger.Warn("failed login attempt", "email", body.Email, "ip", r.RemoteAddr)
		a.auditAuth(r, "auth.login", store.AuditResultFailure, 0, body.Email, nil)
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized,
			"invalid email or password")
		return
	case err != nil:
		a.internalError(w, r, "login", err)
		return
	}

	// Session rotation happens by construction: Login always mints a NEW session
	// (PRD §698). There is no path where a pre-login session id survives the
	// login, so session fixation has nothing to fix onto.
	a.Sessions.SetCookies(w, token, sess.CSRFToken)
	a.Logger.Info("session opened", "user", sess.Email, "team_id", sess.TeamID)
	teamID := sess.TeamID
	a.auditAuth(r, "auth.login", store.AuditResultSuccess, sess.UserID, sess.Email, &teamID)

	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"email":      sess.Email,
		"name":       sess.Name,
		"role":       string(sess.Role),
		"csrf_token": sess.CSRFToken,
		"expires_at": time.Now().Add(session.Lifetime).UTC(),
	})
}

// Logout implements POST /auth/logout: the session is revoked SERVER-SIDE, not
// merely forgotten by the browser. A logout that only clears a cookie leaves a
// valid session behind — that is not a logout, it is a UI gesture.
func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	// Resolve who is logging out BEFORE revoking, so the audit entry names them.
	if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
		a.auditAuth(r, "auth.logout", store.AuditResultSuccess, sess.UserID, sess.Email, sess.CurrentTeamID)
	}
	a.Sessions.Logout(r.Context(), r)
	a.Sessions.ClearCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// Me implements GET /auth/me: who the current cookie belongs to. The dashboard
// calls it on boot to decide between the sign-in page and the app, without
// having to store anything itself.
func (a *API) Me(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	identity := a.Sessions.Authenticate(r.Context(), r)
	if identity == nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "no active session")
		return
	}
	// The CSRF token is handed back so a page reloaded from cache can keep
	// mutating without a full login round trip.
	csrf := ""
	if cookie, err := r.Cookie(session.CSRFCookieName); err == nil {
		csrf = cookie.Value
	}
	// Who the user is, for the dashboard chrome. Authenticate already resolved
	// the session; this second lookup only adds the display identity.
	email, name := "", ""
	if sess, err := a.Sessions.SessionFromRequest(r.Context(), r); err == nil {
		email, name = sess.Email, sess.UserName
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"team_uuid":                identity.TeamUUID,
		"permissions":              identity.Permissions,
		"instance_root":            identity.InstanceRoot,
		"csrf_token":               csrf,
		"email":                    email,
		"name":                     name,
		"mfa_enrollment_required":  identity.MFAPending,
	})
}
