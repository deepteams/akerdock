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
	if host == "" {
		http.Error(w, "missing X-Forwarded-Host", http.StatusBadRequest)
		return
	}
	preview, err := a.Store.GetPreviewByHost(r.Context(), host)
	if err != nil {
		// Not a preview host: nothing to protect here — fail CLOSED anyway,
		// this endpoint exists only behind preview routers.
		http.Error(w, "unknown preview host", http.StatusForbidden)
		return
	}

	// A valid cookie is the fast path — one indexed lookup per request.
	if cookie, err := r.Cookie(previewCookieName); err == nil && cookie.Value != "" {
		token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(cookie.Value))
		if err == nil && token.PreviewID == preview.ID {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// One-shot bootstrap: the authorize endpoint sent the browser back with
	// the token in the query — set the preview-scoped cookie and strip it.
	query := original.Query()
	if raw := query.Get(previewTokenParam); raw != "" {
		token, err := a.Store.GetPreviewAccessTokenByHash(r.Context(), hashPreviewToken(raw))
		if err == nil && token.PreviewID == preview.ID {
			http.SetCookie(w, &http.Cookie{
				Name: previewCookieName, Value: raw, Path: "/",
				Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
				Expires: token.ExpiresAt.Time,
			})
			query.Del(previewTokenParam)
			original.RawQuery = query.Encode()
			http.Redirect(w, r, original.String(), http.StatusFound)
			return
		}
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

	query := target.Query()
	query.Set(previewTokenParam, token)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}
