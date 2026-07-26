package auth

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

// lastUsedGranularity bounds the lazy last_used_at updates (data-dictionary
// §4.8: never on every request).
const lastUsedGranularity = 5 * time.Minute

// Middleware authenticates bearer tokens and enforces the api_enabled gate.
type Middleware struct {
	Store    TokenStore
	Settings SettingsSource
	// Sessions lets the dashboard authenticate with a cookie instead of a
	// bearer token. Nil disables cookie auth entirely.
	Sessions SessionAuthenticator
	Logger   *slog.Logger
}

// TokenStore is the token-lookup boundary for bearer authentication.
type TokenStore interface {
	GetActiveApiTokensByPrefix(context.Context, string) ([]store.GetActiveApiTokensByPrefixRow, error)
	TouchApiTokenLastUsed(context.Context, int64) error
}

// SettingsSource is what the middleware needs from the instance settings
// cache — an interface for the same reason as SessionAuthenticator: so a
// test can inject a fake.
type SettingsSource interface {
	Get(ctx context.Context) (store.InstanceSetting, error)
}

// SessionAuthenticator is what the middleware needs from a session manager. It
// is an interface so internal/auth does not depend on internal/session (which
// depends on internal/auth) — and so a test can inject a fake.
type SessionAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) *Identity
	VerifyCSRF(ctx context.Context, r *http.Request) error
}

// Handler wraps the API routes. GET /api/v1/health stays unauthenticated
// and available even when the API is disabled (§6.6); POST
// /system/api/enable is authenticated but exempt from the gate, so a root
// token can re-enable the API (OpenAPI preamble).
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		identity := m.authenticate(w, r)
		if identity == nil {
			return // response already written
		}

		if !apiGateExempt(identity, r.URL.Path) {
			settings, err := m.Settings.Get(r.Context())
			if err != nil {
				httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
				return
			}
			if !settings.ApiEnabled {
				httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "the API is disabled — enable it in the instance settings (PRD §10.3)")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

// apiGateExempt reports whether a request may proceed while the public API
// is disabled. The gate governs bearer tokens only (PRD §10.3): the
// dashboard session keeps working — the instance settings that re-enable
// the API live behind it — and the re-enable endpoint stays reachable for
// root tokens.
func apiGateExempt(identity *Identity, path string) bool {
	return identity.Session || path == "/api/v1/system/api/enable"
}

func (m *Middleware) authenticate(w http.ResponseWriter, r *http.Request) *Identity {
	unauthorized := func() *Identity {
		w.Header().Set("WWW-Authenticate", `Bearer realm="akerdock"`)
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing or invalid bearer token")
		return nil
	}

	token := SplitBearer(r.Header.Get("Authorization"))
	if token == "" {
		// No bearer token: fall back to the browser session cookie. The
		// dashboard is a first-class client of the same API — it just comes in
		// through a different door (PRD §698).
		if m.Sessions != nil {
			if identity := m.Sessions.Authenticate(r.Context(), r); identity != nil {
				// A cookie is attached by the browser to EVERY request to this
				// origin, including one another site triggered: it proves which
				// browser is calling, never that the user meant to call. So a
				// state-changing request must also echo the CSRF token, which
				// only our own page can read.
				if err := m.Sessions.VerifyCSRF(r.Context(), r); err != nil {
					httpapi.WriteError(w, r, http.StatusForbidden, "csrf_failed",
						"missing or invalid CSRF token — echo the akerdock_csrf cookie in the X-CSRF-Token header")
					return nil
				}
				return identity
			}
		}
		return unauthorized()
	}

	// Prefix pre-filter then constant-time hash comparison (ERD §12).
	candidates, err := m.Store.GetActiveApiTokensByPrefix(r.Context(), token[:PrefixLen])
	if err != nil {
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
		return nil
	}
	hash := HashToken(token)
	var match *store.GetActiveApiTokensByPrefixRow
	for i := range candidates {
		if HashEqual(hash, candidates[i].TokenHash) {
			match = &candidates[i]
			break
		}
	}
	if match == nil {
		return unauthorized()
	}
	if match.ExpiresAt.Valid && time.Now().After(match.ExpiresAt.Time) {
		return unauthorized()
	}
	if len(match.IpAllowlist) > 0 && !ipAllowed(r, match.IpAllowlist) {
		return unauthorized()
	}

	if !match.LastUsedAt.Valid || time.Since(match.LastUsedAt.Time) > lastUsedGranularity {
		go func(id int64) {
			ctx, cancel := contextWithTimeout()
			defer cancel()
			if err := m.Store.TouchApiTokenLastUsed(ctx, id); err != nil {
				m.Logger.Warn("failed to touch token last_used_at", "error", err)
			}
		}(match.ID)
	}

	return &Identity{
		TokenID:   match.ID,
		TokenUUID: uuidString(match.Uuid),
		TeamID:    match.TeamID,
		TeamUUID:  uuidString(match.TeamUuid),
		Display:   match.Name,
		// The token's coarse scopes are expanded to the granular set it holds,
		// keeping the coarse strings too (ADR-038 migration): converted endpoints
		// check a granular permission, the rest still check the coarse one.
		Permissions: EffectivePermissions(match.Permissions),
	}
}

func ipAllowed(r *http.Request, allowlist []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, prefix := range allowlist {
		if prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}
