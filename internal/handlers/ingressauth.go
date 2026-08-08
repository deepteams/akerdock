// Ingress SSO wall (ADR-060 §5): the ADR-030/042 forwardAuth ritual applied
// to an ingress endpoint. Identical shape to ApplicationForwardAuth — the
// resource reference travels in the middleware address, the panel session is
// authoritative, the cookie is minted on the endpoint's own host through a
// reserved callback router — with the endpoint as the protected resource.
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

// ingressCookieName carries the access token on the ingress host — distinct
// from the application and preview cookies so no wall borrows another's grant.
const ingressCookieName = "akerdock_ingress"

// IngressForwardAuth answers Traefik's forwardAuth for sso-walled ingress
// endpoints. Never bearer-authenticated: Traefik is the caller.
func (a *API) IngressForwardAuth(w http.ResponseWriter, r *http.Request) {
	host, original := forwardedURL(r)
	ref := r.URL.Query().Get("endpoint")
	if ref == "" {
		http.Error(w, "missing endpoint reference", http.StatusBadRequest)
		return
	}
	var u pgtype.UUID
	if err := u.Scan(ref); err != nil {
		http.Error(w, "invalid endpoint reference", http.StatusForbidden)
		return
	}
	endpoint, err := a.Store.GetIngressEndpointByUUIDGlobal(r.Context(), u)
	if err != nil {
		// Fail closed: this endpoint exists only behind a walled router.
		http.Error(w, "unknown endpoint", http.StatusForbidden)
		return
	}
	if endpoint.Access != store.IngressAccessSso {
		http.Error(w, "endpoint is not sso walled", http.StatusForbidden)
		return
	}

	// Fast path: a valid cookie scoped to THIS endpoint.
	if cookie, err := r.Cookie(ingressCookieName); err == nil && cookie.Value != "" {
		token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(cookie.Value))
		if err == nil && token.IngressEndpointID != nil && *token.IngressEndpointID == endpoint.ID {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Only top-level navigations get the login dance; a fetch/XHR gets a clean
	// 401 it can react to.
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" && mode != "navigate" {
		http.Error(w, "authentication required — reload the page", http.StatusUnauthorized)
		return
	}
	settings, err := a.Settings.Get(r.Context())
	if err != nil || settings.Fqdn == nil || *settings.Fqdn == "" {
		http.Error(w, "ingress sso protection requires the instance FQDN", http.StatusForbidden)
		return
	}
	if host != "" {
		original.Host = host
	}
	authorize := url.URL{
		Scheme: "https", Host: *settings.Fqdn, Path: "/webhooks/ingress/authorize",
		RawQuery: url.Values{"redirect": {original.String()}}.Encode(),
	}
	http.Redirect(w, r, authorize.String(), http.StatusFound)
}

// IngressAuthorize runs on the PANEL origin, where the AkerDock session lives:
// it verifies the session and the team, mints the access token, and sends the
// browser back to the ingress host.
func (a *API) IngressAuthorize(w http.ResponseWriter, r *http.Request) {
	target, err := url.Parse(r.URL.Query().Get("redirect"))
	if err != nil || target.Host == "" || target.Scheme != "https" {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	// The redirect host must BE a declared ingress endpoint — the sole
	// anti-open-redirect rule.
	endpoint, err := a.Store.GetIngressEndpointByFQDN(r.Context(), target.Hostname())
	if err != nil {
		http.Error(w, "unknown endpoint host", http.StatusNotFound)
		return
	}
	if endpoint.Access != store.IngressAccessSso {
		http.Error(w, "endpoint is not sso walled", http.StatusForbidden)
		return
	}
	if a.Sessions == nil {
		http.Error(w, "sessions are not available on this deployment", http.StatusConflict)
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !id.CanAccessTeam(endpoint.TeamID) {
		http.Error(w, "this endpoint belongs to another team", http.StatusForbidden)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(raw)
	_ = a.Store.DeleteExpiredPreviewAccessTokens(r.Context())
	if err := a.Store.CreateIngressAccessToken(r.Context(), store.CreateIngressAccessTokenParams{
		TokenHash:         hashPreviewToken(token),
		IngressEndpointID: &endpoint.ID,
		ExpiresAt:         pgtype.Timestamptz{Time: time.Now().Add(previewAccessTTL), Valid: true},
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.recordAudit(r, id, "ingress.access", "ingress_endpoint", endpoint.Uuid)

	next := target.EscapedPath()
	if next == "" {
		next = "/"
	}
	if target.RawQuery != "" {
		next += "?" + target.RawQuery
	}
	callback := url.URL{
		Scheme: "https", Host: target.Host, Path: "/.akerdock/ingress-callback",
		RawQuery: url.Values{"token": {token}, "next": {next}}.Encode(),
	}
	http.Redirect(w, r, callback.String(), http.StatusFound)
}

// IngressCallback lands on the ingress host and turns the one-shot token into
// the scoped cookie. `next` is constrained to a local path.
func (a *API) IngressCallback(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("token")
	if raw == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(raw))
	if err != nil || token.IngressEndpointID == nil {
		http.Error(w, "invalid or expired access token — reopen the URL", http.StatusForbidden)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: ingressCookieName, Value: raw, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: token.ExpiresAt.Time,
	})
	http.Redirect(w, r, next, http.StatusFound)
}
