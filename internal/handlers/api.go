// Package handlers implements the operations of the public API over the
// oapi-codegen generated router. Unimplemented operations answer 501 via
// the embedded generated stub.
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// API carries the dependencies of the implemented operations.
type API struct {
	// Sessions authenticates the dashboard's browser session (PRD §698). Nil in
	// API-only deployments: the /auth routes then answer 404.
	Sessions *session.Manager
	// Passkeys is the WebAuthn engine behind /auth/passkeys/* and the passkey
	// login. Nil when sessions are nil, or when no relying party could be
	// configured.
	Passkeys *session.Passkeys
	// MFA is the TOTP engine behind /auth/mfa/* (PRD §10.2). Nil when
	// sessions are nil or no keyring is available: 2FA guards the dashboard
	// login, and without sessions there is no dashboard login to guard.
	MFA *session.TOTP
	// OAuth is the federated login engine behind /auth/oauth/* (§10.2).
	// Nil in API-only deployments, like Sessions.
	OAuth *session.OAuth
	api.Unimplemented

	Store    *store.Queries
	Pool     handlerPool
	Settings *instance.Cache
	Keyring  *envelope.Keyring
	Audit    *audit.Recorder
	Events   *events.Broker
	Version  string
	Logger   *slog.Logger

	// Terminal session bounds (§24.4) — AKERDOCK_TERMINAL_IDLE_TIMEOUT and
	// AKERDOCK_TERMINAL_MAX_DURATION; zero falls back to the defaults.
	TerminalIdleTimeout time.Duration
	TerminalMaxDuration time.Duration
}

// handlerPool is the small transaction/health boundary used by the HTTP
// layer. Keeping the concrete pgx pool behind this interface lets handler
// tests exercise complete request flows without starting PostgreSQL.
type handlerPool interface {
	Begin(context.Context) (pgx.Tx, error)
	Ping(context.Context) error
}

// recordAudit appends to the audit trail (§23.4); failures are logged by
// the recorder and never fail the audited operation.
func (a *API) recordAudit(r *http.Request, id *auth.Identity, action, targetKind string, target pgtype.UUID) {
	a.Audit.Record(r, id, audit.Event{Action: action, TargetKind: targetKind, TargetUUID: target})
}

// recordAuditDiff audits a modification with what actually changed (§23.4).
// The diff is redacted by audit.Diff: a sensitive field is reported as changed,
// never with its value — the audit table is kept forever and exported, so a
// secret written into it is a second copy of that secret.
func (a *API) recordAuditDiff(r *http.Request, id *auth.Identity, action, targetKind string, target pgtype.UUID, before, after map[string]any) {
	a.Audit.Record(r, id, audit.Event{
		Action: action, TargetKind: targetKind, TargetUUID: target,
		Diff: audit.Diff(before, after),
	})
}

// auditAuth records an authentication event (§23.4: login/logout/failures/MFA).
// userID is the acting user when known (0 on a failed login that resolved
// nobody); display is a human identifier shown then — typically the attempted
// email. Nil Audit (some tests) makes it a no-op.
func (a *API) auditAuth(r *http.Request, action string, result store.AuditResult, userID int64, display string, teamID *int64) {
	if a.Audit == nil {
		return
	}
	var actorUUID pgtype.UUID
	if userID != 0 {
		actorUUID = userUUIDOf(a, r, userID)
	}
	a.Audit.RecordAuth(r, action, result, actorUUID, display, teamID)
}

// NewRouter assembles the public API router: request ids, panic recovery,
// bearer authentication, then the cross-cutting policies of §24.1 — rate
// limiting (200 req/min per token) and Idempotency-Key handling — and the
// generated operation routes under /api/v1.
func NewRouter(a *API, mw *auth.Middleware) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(a.requestContext)
	r.Use(recoverJSON(a.Logger))

	limiter := httpapi.NewLimiter()
	rateLimit := limiter.Handler(func(req *http.Request) string {
		// Budget per token; unauthenticated routes (health) are exempt.
		if id, ok := auth.FromContext(req.Context()); ok {
			return id.TokenUUID
		}
		return ""
	})
	idempotency := (&httpapi.Idempotency{Store: a.Store}).Handler(func(req *http.Request) (int64, bool) {
		if id, ok := auth.FromContext(req.Context()); ok {
			return id.TeamID, true
		}
		return 0, false
	})

	// Git webhooks live OUTSIDE /api/v1 and outside the bearer middleware
	// (git-webhook-protocols §1.1): they are authenticated by the provider's
	// signature, not by a token. They are mounted on the base router, before the
	// generated routes, so no auth middleware ever sees them.
	r.Post("/webhooks/{provider}/{endpoint_uuid}", a.ReceiveGitWebhook)

	// GitHub App plumbing (git-webhook-protocols §2.1/§2.4): the manifest
	// callback and the setup return are BROWSER redirects, the app webhook is
	// GitHub calling — all authenticated by state or signature, never bearer.
	r.Get("/webhooks/github/manifest/callback", a.GithubManifestCallback)
	r.Get("/webhooks/github/apps/{app_uuid}/setup", a.GithubAppSetup)
	r.Post("/webhooks/github/apps/{app_uuid}", a.ReceiveGithubAppWebhook)

	// Agent ingestion (ADR-040): the server helpers pushing observation
	// batches, authenticated by their per-server token — never a user bearer,
	// hence outside /api/v1 like the webhooks. Its own limiter bounds a
	// broken or hostile agent per token.
	agentLimit := httpapi.NewLimiterRate(300).Handler(func(req *http.Request) string {
		return req.Header.Get("Authorization")
	})
	r.With(agentLimit).Post("/agent/v1/observations", a.AgentObservations)

	// Preview SSO (ADR-030): forward-auth is Traefik calling per request,
	// authorize is a BROWSER redirect authenticated by the panel session —
	// neither carries a bearer token.
	r.Get("/webhooks/previews/forward-auth", a.PreviewForwardAuth)
	r.Get("/webhooks/previews/authorize", a.PreviewAuthorize)
	// The cookie bootstrap of a preview: reached under the PREVIEW's host,
	// proxied here by its dedicated router (ADR-030).
	r.Get("/.akerdock/preview-callback", a.PreviewCallback)

	// Browser authentication (PRD §698). Outside /api/v1 and outside the bearer
	// middleware: the v1 contract knows nothing of sessions (§10.2), and these
	// routes exist for the dashboard alone.
	//
	// The whole group sits behind a per-IP limiter, MUCH tighter than the API
	// budget: /auth/login and the passkey finish are the endpoints that turn a
	// guess into an answer, and they are reachable without any credential. The
	// account lockout bounds guesses per account; this bounds them per source.
	authLimit := httpapi.NewLimiterRate(httpapi.AuthRatePerMinute).Handler(httpapi.ClientIPKey)
	r.Group(func(r chi.Router) {
		r.Use(authLimit)
		r.Post("/auth/login", a.Login)
		r.Post("/auth/logout", a.Logout)
		r.Get("/auth/me", a.Me)

		// Redeem an invitation link (ADR-038): a signed-in invitee joins the team.
		// Session + CSRF like the rest of the mutating /auth endpoints.
		r.Post("/auth/invitations/accept", a.AcceptInvitation)

		// Passkeys (WebAuthn). Management requires a session (and CSRF); the
		// login pair is anonymous by nature — a discoverable credential names
		// its user.
		r.Post("/auth/passkeys/register/begin", a.BeginPasskeyRegistration)
		r.Post("/auth/passkeys/register/finish", a.FinishPasskeyRegistration)
		r.Get("/auth/passkeys", a.ListPasskeys)
		r.Delete("/auth/passkeys/{passkey_uuid}", a.DeletePasskey)
		r.Post("/auth/passkey/login/begin", a.BeginPasskeyLogin)
		r.Post("/auth/passkey/login/finish", a.FinishPasskeyLogin)

		// Step-up re-authentication (rbac-matrix §5): a session proving it
		// still holds its passkey before a sensitive action.
		r.Post("/auth/passkey/stepup/begin", a.BeginPasskeyStepUp)
		r.Post("/auth/passkey/stepup/finish", a.FinishPasskeyStepUp)

		// TOTP 2FA (PRD §10.2). /auth/mfa/verify is step two of the login and
		// belongs behind this limiter as much as /auth/login does: it is the
		// other endpoint that turns a guess into an answer. The rest manages
		// the factor under session + CSRF.
		r.Post("/auth/mfa/verify", a.VerifyMFALogin)
		r.Get("/auth/mfa", a.MFAStatus)
		r.Post("/auth/mfa/totp/setup", a.SetupMFATOTP)
		r.Post("/auth/mfa/totp/confirm", a.ConfirmMFATOTP)
		r.Delete("/auth/mfa/totp", a.DisableMFATOTP)
		r.Post("/auth/mfa/recovery-codes", a.RegenerateMFARecoveryCodes)

		// OAuth/OIDC login (§10.2). The start and the callback are the two
		// endpoints that answer without a credential — behind this limiter
		// with the rest. The callback is a top-level browser navigation from
		// the identity provider, hence a GET answering redirects.
		r.Get("/auth/oauth/providers", a.OauthProviders)
		r.Post("/auth/oauth/{oauth_provider}/start", a.StartOauth)
		r.Get("/auth/oauth/{oauth_provider}/callback", a.OauthCallback)
		r.Get("/auth/identities", a.ListIdentities)
		r.Delete("/auth/identities/{identity_uuid}", a.DeleteIdentity)

		// CLI login (ADR-031) — out of contract like the rest of /auth.
		// start/token answer without a credential (behind the per-IP limiter);
		// approve/request require the panel session (+ CSRF on approve).
		r.Post("/auth/cli/start", a.CliAuthStart)
		r.Get("/auth/cli/request", a.CliAuthRequest)
		r.Post("/auth/cli/approve", a.CliAuthApprove)
		r.Post("/auth/cli/token", a.CliAuthToken)

		// The terminal WebSocket (§24.4, ADR-024) — outside the contract like
		// /auth: authenticated by its single-use attach token, minted by the
		// POST .../terminal-sessions operations. Behind the same per-IP
		// limiter: this endpoint too answers without a bearer credential.
		// SCIM 2.0 provisioning (ADR-038 bis) — outside /api/v1 like /auth: it
		// speaks the SCIM dialect and authenticates with a per-team SCIM token.
		r.Get("/scim/v2/ServiceProviderConfig", a.ScimServiceProviderConfig)
		r.Get("/scim/v2/Users", a.ScimListUsers)
		r.Post("/scim/v2/Users", a.ScimCreateUser)
		r.Get("/scim/v2/Users/{id}", a.ScimGetUser)
		r.Put("/scim/v2/Users/{id}", a.ScimReplaceUser)
		r.Patch("/scim/v2/Users/{id}", a.ScimPatchUser)
		r.Delete("/scim/v2/Users/{id}", a.ScimDeleteUser)
		r.Get("/scim/v2/Groups", a.ScimListGroups)
		r.Post("/scim/v2/Groups", a.ScimCreateGroup)
		r.Get("/scim/v2/Groups/{id}", a.ScimGetGroup)
		r.Patch("/scim/v2/Groups/{id}", a.ScimPatchGroup)

		r.Get("/terminal/ws", a.TerminalWebSocket)
		// The CLI TCP tunnel WebSocket (ADR-032), same contract as the
		// terminal: single-use attach token minted by POST .../port-forwards.
		r.Get("/tunnel/ws", a.TunnelWebSocket)
	})

	return api.HandlerWithOptions(a, api.ChiServerOptions{
		BaseURL:    "/api/v1",
		BaseRouter: r,
		// The generated wrapper applies middlewares in reverse order (the last
		// entry ends up outermost), so authentication must come last: rate
		// limiting and idempotency both need the authenticated identity. The body
		// cap is innermost — it only needs to wrap r.Body before a handler reads
		// it (§23.3: a JSON API request has no business being megabytes).
		Middlewares: []api.MiddlewareFunc{bodyLimit, idempotency, rateLimit, mw.Handler},
		ErrorHandlerFunc: func(w http.ResponseWriter, req *http.Request, err error) {
			httpapi.WriteError(w, req, http.StatusBadRequest, httpapi.CodeBadRequest, err.Error())
		},
	})
}

// maxRequestBody caps the request body of a /api/v1 operation (§23.3). JSON
// payloads are kilobytes; 5 MiB leaves generous room for the largest legitimate
// body (config-as-code, key material) while bounding a memory-exhaustion attempt.
// The generated handlers decode without their own limit, so this is their guard.
const maxRequestBody = 5 << 20

// bodyLimit wraps the request body so an over-large payload fails with 413
// (via http.MaxBytesReader) instead of being read unbounded into memory. Scoped
// to /api/v1: git webhooks (larger provider payloads) and /auth (its own tighter
// per-endpoint limits) are mounted elsewhere and unaffected.
func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

// requestContext stamps each request with a UUID request id (surfaced as the
// X-Request-Id response header and recorded on every audit row, §23.4) and a
// correlation id — a client-supplied X-Correlation-Id when a valid UUID, else
// the request id — so a chain of related calls can be tied together in the
// audit trail.
func (a *API) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID, err := pguuid.New()
		if err == nil {
			ctx := audit.WithRequestID(r.Context(), reqID)
			corr := reqID
			if h := r.Header.Get("X-Correlation-Id"); h != "" {
				var c pgtype.UUID
				if c.Scan(h) == nil {
					corr = c
				}
			}
			ctx = audit.WithCorrelationID(ctx, corr)
			w.Header().Set("X-Request-Id", pguuid.String(reqID))
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// recoverJSON turns panics into the Error schema, without a stack trace in
// the response (§24.1).
func recoverJSON(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic in handler", "panic", rec, "path", r.URL.Path)
					httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// require checks the operation's x-required-permission and writes the 401
// or 403 response itself when the check fails.
func (a *API) require(w http.ResponseWriter, r *http.Request, perm auth.Permission) (*auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing or invalid bearer token")
		return nil, false
	}
	if id.MFAPending {
		// Forced MFA enrollment (§10.2): the session may only enroll a factor
		// (the /auth/mfa/* endpoints, which do not go through require). Every API
		// operation is refused until it does.
		httpapi.WriteError(w, r, http.StatusForbidden, "mfa_enrollment_required",
			"this instance requires two-factor authentication — enrol a factor to continue")
		return nil, false
	}
	if !auth.Has(id.Permissions, perm) {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "this operation requires the "+string(perm)+" permission")
		return nil, false
	}
	return id, true
}

// requireInstanceRoot gates instance-wide settings (/system/*): only a session
// of the instance root user (users.is_root, established at bootstrap) may touch
// them — the platform administrator, OUTSIDE the team-role model (rbac-matrix
// §3.5). A team owner/admin's team-scoped `root` permission is NOT enough, and
// API tokens are team-bound so are always refused here (§3.5).
func (a *API) requireInstanceRoot(w http.ResponseWriter, r *http.Request) (*auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "missing or invalid bearer token")
		return nil, false
	}
	if !id.InstanceRoot {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "this operation is reserved to the instance administrator")
		return nil, false
	}
	if id.MFAPending {
		httpapi.WriteError(w, r, http.StatusForbidden, "mfa_enrollment_required",
			"this instance requires two-factor authentication — enrol a factor to continue")
		return nil, false
	}
	return id, true
}

// resolveTeam loads a team by public UUID and applies team isolation: a
// valid UUID belonging to another team yields the same 404 as a missing one
// (INV-002).
func (a *API) resolveTeam(w http.ResponseWriter, r *http.Request, id *auth.Identity, teamUUID string) (store.Team, bool) {
	var u pgtype.UUID
	if err := u.Scan(teamUUID); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "team not found")
		return store.Team{}, false
	}
	team, err := a.Store.GetTeamByUUID(r.Context(), u)
	if err != nil || !id.CanAccessTeam(team.ID) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "team not found")
		return store.Team{}, false
	}
	return team, true
}
