// Application access wall (ADR-042): the preview mechanism of ADR-030 applied
// to a production application — Traefik forwardAuth delegates each request to
// the control plane, which trusts the AkerDock session and the team
// membership, then hands the browser an application-scoped cookie.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

// applicationCookieName carries the access token on the APPLICATION domain —
// distinct from the preview cookie so the two walls never borrow each other's
// grants.
const applicationCookieName = "akerdock_app"

// generateBasicAuthCredentials mints "user:password" for an application's
// basic-auth wall (ADR-042) — the same shape as the generated preview secret,
// so both walls read alike wherever they are revealed.
func generateBasicAuthCredentials() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "akerdock:" + hex.EncodeToString(raw), nil
}

// ApplicationForwardAuth answers Traefik's forwardAuth for sso-protected
// applications: 200 with a valid cookie, redirect to the panel's authorize
// endpoint otherwise. Never bearer-authenticated: Traefik is the caller.
func (a *API) ApplicationForwardAuth(w http.ResponseWriter, r *http.Request) {
	host, original := forwardedURL(r)
	// The identity travels in the middleware ADDRESS (?application=…): the
	// auth call may transit other proxies that rewrite X-Forwarded-Host, but
	// a query parameter survives every hop (ADR-030's lesson).
	var app store.GetApplicationAccessByUUIDRow
	if raw := r.URL.Query().Get("application"); raw != "" {
		var id pgtype.UUID
		if err := id.Scan(raw); err != nil {
			http.Error(w, "invalid application reference", http.StatusForbidden)
			return
		}
		row, err := a.Store.GetApplicationAccessByUUID(r.Context(), id)
		if err != nil {
			// Fail CLOSED: this endpoint exists only behind protected routers.
			http.Error(w, "unknown application", http.StatusForbidden)
			return
		}
		app = row
	} else {
		http.Error(w, "missing application reference", http.StatusBadRequest)
		return
	}

	// A valid cookie is the fast path — one indexed lookup per request.
	if cookie, err := r.Cookie(applicationCookieName); err == nil && cookie.Value != "" {
		token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(cookie.Value))
		if err == nil && token.ApplicationID != nil && *token.ApplicationID == app.ID {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// UNAUTHENTICATED from here on. Only top-level NAVIGATIONS get the login
	// dance: a fetch/XHR cannot complete a cross-origin redirect ritual. A
	// clean 401 (never WWW-Authenticate) lets the app's own code react.
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" && mode != "navigate" {
		http.Error(w, "authentication required — reload the page", http.StatusUnauthorized)
		return
	}

	settings, err := a.Settings.Get(r.Context())
	if err != nil || settings.Fqdn == nil || *settings.Fqdn == "" {
		http.Error(w, "application sso protection requires the instance FQDN", http.StatusForbidden)
		return
	}
	if host != "" {
		original.Host = host
	}
	authorize := url.URL{
		Scheme: "https", Host: *settings.Fqdn, Path: "/webhooks/applications/authorize",
		RawQuery: url.Values{"redirect": {original.String()}}.Encode(),
	}
	http.Redirect(w, r, authorize.String(), http.StatusFound)
}

// ApplicationAuthorize runs on the PANEL origin, where the AkerDock session
// cookie lives: it verifies the session and the team (INV-001), mints the
// access token and sends the browser back to the application's host.
func (a *API) ApplicationAuthorize(w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(r.URL.Query().Get("redirect"))
	if err != nil || target.Host == "" || target.Scheme != "https" {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	// The redirect host must BE a routed host of a known application — the
	// sole anti open-redirect rule that matters here.
	app, err := a.Store.GetApplicationByRoutedHost(r.Context(), target.Host)
	if err != nil {
		http.Error(w, "unknown application host", http.StatusNotFound)
		return
	}
	if a.Sessions == nil {
		http.Error(w, "sessions are not available on this deployment", http.StatusConflict)
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil {
		// No session: to the login page. The user retries the link once
		// logged in — the session then answers on the first pass.
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !id.CanAccessTeam(app.TeamID) {
		http.Error(w, "this application belongs to another team", http.StatusForbidden)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(raw)
	_ = a.Store.DeleteExpiredPreviewAccessTokens(r.Context())
	if err := a.Store.CreateApplicationAccessToken(r.Context(), store.CreateApplicationAccessTokenParams{
		TokenHash: hashPreviewToken(token), ApplicationID: &app.ID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(previewAccessTTL), Valid: true},
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.recordAudit(r, id, "application.access", "application", app.Uuid)
	a.Logger.Info("application access granted", "host", target.Host)

	// The cookie bootstrap happens on the APPLICATION's own host, through its
	// dedicated callback router: the token rides the request URL — query
	// strings survive every proxy hop, X-Forwarded-* headers do not.
	next := target.EscapedPath()
	if next == "" {
		next = "/"
	}
	if target.RawQuery != "" {
		next += "?" + target.RawQuery
	}
	callback := url.URL{
		Scheme: "https", Host: target.Host, Path: "/.akerdock/app-callback",
		RawQuery: url.Values{"token": {token}, "next": {next}}.Encode(),
	}
	http.Redirect(w, r, callback.String(), http.StatusFound)
}

// ApplicationCallback lands on the APPLICATION host and turns the one-shot
// token into the scoped cookie. `next` is constrained to a local path: this
// endpoint never becomes an open redirect.
func (a *API) ApplicationCallback(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	if raw == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(raw))
	if err != nil || token.ApplicationID == nil {
		http.Error(w, "invalid or expired access token — reopen the application URL", http.StatusForbidden)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: applicationCookieName, Value: raw, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: token.ExpiresAt.Time,
	})
	http.Redirect(w, r, next, http.StatusFound)
}
