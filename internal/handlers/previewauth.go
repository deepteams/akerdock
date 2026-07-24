// Preview SSO (ADR-030): Traefik forwardAuth delegates each preview request
// to the control plane, which trusts the AKERDOCK session — whatever login
// method produced it (password, passkey, OIDC). The preview holds its own
// scoped cookie; the panel session never leaves the panel's origin.
package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

// previewCookieName carries the preview access token on the PREVIEW domain.
const previewCookieName = "akerdock_preview"

// previewTokenParam is the one-shot query parameter of the cookie bootstrap.
const previewTokenParam = "akerdock_preview_token"

// previewAccessTTL bounds one authorization; re-authorizing is a redirect
// round-trip through the (still open) panel session.
const previewAccessTTL = 12 * time.Hour

func hashPreviewToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// forwardedURL reconstructs the URL the browser asked for, from the headers
// Traefik sets on the forwardAuth call.
func forwardedURL(r *http.Request) (host string, original *url.URL) {
	host = r.Header.Get("X-Forwarded-Host")
	uri := r.Header.Get("X-Forwarded-Uri")
	if uri == "" {
		uri = "/"
	}
	u, err := url.Parse("https://" + host + uri)
	if err != nil {
		return host, &url.URL{Scheme: "https", Host: host, Path: "/"}
	}
	return host, u
}

// PreviewForwardAuth answers Traefik's forwardAuth for sso-protected
// previews: 200 with a valid preview cookie, cookie bootstrap when the
// one-shot token arrives, and a redirect to the panel's authorize endpoint
// otherwise. Never bearer-authenticated: Traefik is the caller.
func (a *API) PreviewForwardAuth(w http.ResponseWriter, r *http.Request) {
	host, original := forwardedURL(r)
	// The preview identity comes from the middleware's ADDRESS (?preview=…):
	// the auth call may transit other proxies — the panel's own router when
	// the instance FQDN loops back through Traefik — and those rewrite
	// X-Forwarded-Host. The address query survives every hop (ADR-030).
	preview, err := store.Preview{}, error(nil)
	if u := r.URL.Query().Get("preview"); u != "" {
		var id pgtype.UUID
		if err := id.Scan(u); err != nil {
			http.Error(w, "invalid preview reference", http.StatusForbidden)
			return
		}
		preview, err = a.Store.GetPreviewByUUID(r.Context(), id)
	} else if host != "" {
		preview, err = a.Store.GetPreviewByHost(r.Context(), host)
	} else {
		http.Error(w, "missing preview reference", http.StatusBadRequest)
		return
	}
	if err != nil {
		// Fail CLOSED: this endpoint exists only behind preview routers.
		http.Error(w, "unknown preview host", http.StatusForbidden)
		return
	}
	// The redirect must land back on the ORIGINAL host. X-Forwarded-Host is
	// trusted only when it belongs to this preview (primary fqdn or a compose
	// service's `<service>-<fqdn>`); rewritten by an intermediate proxy, it
	// falls back to the preview's primary fqdn.
	if preview.Fqdn != nil && !previewOwnsHost(*preview.Fqdn, host) {
		original.Host = *preview.Fqdn
	}

	// A valid cookie is the fast path — one indexed lookup per request.
	if cookie, err := r.Cookie(previewCookieName); err == nil && cookie.Value != "" {
		token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(cookie.Value))
		if err == nil && token.PreviewID == preview.ID {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Only top-level NAVIGATIONS get the login dance: a fetch/XHR cannot
	// complete a cross-origin redirect ritual — it only drowns in CORS noise.
	// A clean 401 lets the app's own code react (and no WWW-Authenticate:
	// the browser must never open a dialog). Sec-Fetch-Mode is browser-set
	// and survives proxies, unlike the X-Forwarded-* family.
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" && mode != "navigate" {
		http.Error(w, "preview authentication required — open the preview URL in the browser", http.StatusUnauthorized)
		return
	}

	settings, err := a.Settings.Get(r.Context())
	if err != nil || settings.Fqdn == nil || *settings.Fqdn == "" {
		http.Error(w, "preview sso requires the instance FQDN", http.StatusForbidden)
		return
	}
	authorize := url.URL{
		Scheme: "https", Host: *settings.Fqdn, Path: "/webhooks/previews/authorize",
		RawQuery: url.Values{"redirect": {original.String()}}.Encode(),
	}
	http.Redirect(w, r, authorize.String(), http.StatusFound)
}

// PreviewAuthorize runs on the PANEL origin, where the AkerDock session
// cookie lives: it verifies the session and the team (INV-001), mints the
// preview access token, audits it, and sends the browser back.
func (a *API) PreviewAuthorize(w http.ResponseWriter, r *http.Request) {
	redirect := r.URL.Query().Get("redirect")
	target, err := url.Parse(redirect)
	if err != nil || target.Host == "" || target.Scheme != "https" {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	// The redirect host must BE a known preview's host — the sole anti
	// open-redirect rule that matters here (ADR-030).
	preview, err := a.Store.GetPreviewByHost(r.Context(), target.Host)
	if err != nil {
		http.Error(w, "unknown preview host", http.StatusNotFound)
		return
	}

	if a.Sessions == nil {
		http.Error(w, "sessions are not available on this deployment", http.StatusConflict)
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil {
		// No session: to the login page. The user retries the preview link
		// once logged in — the session then answers on the first pass.
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	app, err := a.Store.GetApplicationByID(r.Context(), preview.ApplicationID)
	if err != nil {
		http.Error(w, "the preview's application no longer exists", http.StatusNotFound)
		return
	}
	if !id.CanAccessTeam(app.Resource.TeamID) {
		http.Error(w, "this preview belongs to another team", http.StatusForbidden)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(raw)
	_ = a.Store.DeleteExpiredPreviewAccessTokens(r.Context())
	if err := a.Store.CreatePreviewAccessToken(r.Context(), store.CreatePreviewAccessTokenParams{
		TokenHash: hashPreviewToken(token), PreviewID: preview.ID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(previewAccessTTL), Valid: true},
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.recordAudit(r, id, "preview.access", "application", app.Resource.Uuid)
	a.Logger.Info("preview access granted", "preview", fmt.Sprint(preview.PrID), "host", target.Host)

	// The cookie bootstrap happens on the PREVIEW's own host, through its
	// dedicated callback router (ADR-030): the token rides the request URL —
	// query strings survive every proxy hop, X-Forwarded-* headers do not.
	next := target.EscapedPath()
	if next == "" {
		next = "/"
	}
	if target.RawQuery != "" {
		next += "?" + target.RawQuery
	}
	callback := url.URL{
		Scheme: "https", Host: target.Host, Path: "/.akerdock/preview-callback",
		RawQuery: url.Values{"token": {token}, "next": {next}}.Encode(),
	}
	http.Redirect(w, r, callback.String(), http.StatusFound)
}

// PreviewCallback lands on the PREVIEW host — its dedicated router proxies
// the path server-side to the control plane — and turns the one-shot token
// into the preview-scoped cookie. `next` is constrained to a local path:
// this endpoint never becomes an open redirect.
func (a *API) PreviewCallback(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	if raw == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(raw))
	if err != nil {
		http.Error(w, "invalid or expired preview access token — reopen the preview URL", http.StatusForbidden)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: previewCookieName, Value: raw, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: token.ExpiresAt.Time,
	})
	http.Redirect(w, r, next, http.StatusFound)
}

// previewOwnsHost reports whether host is one of the preview's own hosts:
// its fqdn, or a compose service's derived `<service>-<fqdn>` (§20.4.1).
func previewOwnsHost(fqdn, host string) bool {
	if host == "" || fqdn == "" {
		return false
	}
	if strings.EqualFold(host, fqdn) {
		return true
	}
	return strings.HasSuffix(strings.ToLower(host), "-"+strings.ToLower(fqdn))
}
