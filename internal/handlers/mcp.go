// MCP surface (ADR-043, PRD §12): Streamable HTTP on /mcp with two
// authenticated paths — an API token (local clients, CI) or an OAuth 2.1
// access token (remote clients). Everything is read-only; the whole surface
// answers 404 until an instance root enables it.
package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	// mcpTokenScheme prefixes MCP access tokens, next to akd_ (API), akdp_
	// (port-forward) and akda_ (agent).
	mcpTokenScheme = "akdm_"
	// mcpTokenTTL bounds one grant. Short by design: an assistant reconnects
	// through the flow, and a leaked token expires on its own.
	mcpTokenTTL = 12 * time.Hour
	// mcpCodeTTL bounds the authorization code round-trip.
	mcpCodeTTL = 5 * time.Minute
	// mcpMaxBody caps one JSON-RPC message.
	mcpMaxBody = 1 << 20
)

func hashMcpToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mcpEnabled reports whether the instance exposes the MCP surface at all.
func (a *API) mcpEnabled(r *http.Request) bool {
	st, err := a.Settings.Get(r.Context())
	return err == nil && st.McpEnabled
}

// mcpDcrEnabled reports whether the instance accepts dynamic client
// registration (ADR-044): off by default, CIMD being the identity path.
func (a *API) mcpDcrEnabled(r *http.Request) bool {
	st, err := a.Settings.Get(r.Context())
	return err == nil && st.McpDcrEnabled
}

// McpEndpoint implements POST/GET /mcp: one JSON-RPC message in, one response
// out (Streamable HTTP). A GET without a stream to open answers 405, which is
// what the protocol prescribes for a server with no server-initiated messages.
func (a *API) McpEndpoint(w http.ResponseWriter, r *http.Request) {
	if !a.mcpEnabled(r) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP server only accepts POST", http.StatusMethodNotAllowed)
		return
	}
	teamID, ok := a.mcpAuthenticate(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody))
	if err != nil {
		http.Error(w, "cannot read the request body", http.StatusBadRequest)
		return
	}
	resp := a.MCP.Handle(r.Context(), teamID, raw)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted) // a notification expects no answer
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// mcpAuthenticate resolves the caller's team from either credential. On
// failure it emits the WWW-Authenticate challenge pointing at the protected
// resource metadata — how an MCP client discovers where to authenticate.
func (a *API) mcpAuthenticate(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if raw == "" {
		a.mcpChallenge(w, r, "a bearer token is required")
		return 0, false
	}
	// An MCP access token (remote client, OAuth).
	if strings.HasPrefix(raw, mcpTokenScheme) {
		token, err := a.Store.GetMcpAccessTokenByHash(r.Context(), hashMcpToken(raw))
		if err != nil {
			a.mcpChallenge(w, r, "invalid or expired token")
			return 0, false
		}
		_ = a.Store.TouchMcpAccessToken(r.Context(), token.ID)
		return token.TeamID, true
	}
	// An API token (local client, CI): it must carry `read` — the MCP
	// surface never exposes more than a viewer sees.
	teamID, ok := a.mcpAPIToken(r, raw)
	if !ok {
		a.mcpChallenge(w, r, "invalid token")
		return 0, false
	}
	return teamID, true
}

func (a *API) mcpChallenge(w http.ResponseWriter, r *http.Request, reason string) {
	if base, ok := a.instanceBaseURL(r); ok {
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
	}
	http.Error(w, reason, http.StatusUnauthorized)
}

// McpProtectedResourceMetadata implements
// GET /.well-known/oauth-protected-resource (RFC 9728): tells an MCP client
// which authorization server guards this resource.
func (a *API) McpProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base, hasBase := a.instanceBaseURL(r)
	if !a.mcpEnabled(r) || !hasBase {
		http.NotFound(w, r)
		return
	}
	writeJSONNoStore(w, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"akerdock:read"},
	})
}

// McpAuthorizationServerMetadata implements
// GET /.well-known/oauth-authorization-server (RFC 8414).
func (a *API) McpAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	base, hasBase := a.instanceBaseURL(r)
	if !a.mcpEnabled(r) || !hasBase {
		http.NotFound(w, r)
		return
	}
	metadata := map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/mcp/authorize",
		"token_endpoint":                        base + "/oauth/mcp/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"akerdock:read"},
		// ADR-044: the preferred identity path — a client_id that is the
		// https URL of the client's metadata document.
		"client_id_metadata_document_supported": true,
	}
	// Advertised only when registration is actually open (ADR-044): a client
	// must not be told to POST somewhere that will refuse it.
	if a.mcpDcrEnabled(r) {
		metadata["registration_endpoint"] = base + "/oauth/mcp/register"
	}
	writeJSONNoStore(w, metadata)
}

// McpRegisterClient implements POST /oauth/mcp/register (RFC 7591): dynamic
// client registration. Open by design — registering grants NOTHING: only a
// user's explicit consent, under their session, mints a token.
func (a *API) McpRegisterClient(w http.ResponseWriter, r *http.Request) {
	if !a.mcpEnabled(r) {
		http.NotFound(w, r)
		return
	}
	// ADR-044: dynamic registration is an explicit instance opt-in. The
	// default identity path is CIMD — a client_id that is the https URL of
	// the client's own metadata document, an identity nobody can forge.
	if !a.mcpDcrEnabled(r) {
		writeOAuthError(w, http.StatusForbidden, "access_denied",
			"dynamic client registration is disabled on this instance — use a Client ID Metadata Document "+
				"(an https client_id serving your client metadata), or ask the instance administrator to enable registration")
		return
	}
	var body struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || len(body.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, uri := range body.RedirectURIs {
		if !validRedirectURI(uri) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect uris must be https, or http on localhost")
			return
		}
	}
	name := body.ClientName
	if name == "" {
		name = "MCP client"
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "cannot mint a client id")
		return
	}
	clientID := "akdmc_" + hex.EncodeToString(raw)
	client, err := a.Store.RegisterMcpOauthClient(r.Context(), store.RegisterMcpOauthClientParams{
		ClientID: clientID, ClientName: name, RedirectUris: body.RedirectURIs,
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "cannot register the client")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSONNoStore(w, map[string]any{
		"client_id":                  client.ClientID,
		"client_name":                client.ClientName,
		"redirect_uris":              client.RedirectUris,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// McpAuthorize implements GET /oauth/mcp/authorize: it validates the request
// and RENDERS THE CONSENT SCREEN. It never grants anything by itself — a
// session alone must not be enough, or any third-party page could send the
// browser here and walk away with a code. The grant happens on the POST
// below, which carries the session's CSRF token.
func (a *API) McpAuthorize(w http.ResponseWriter, r *http.Request) {
	req, client, ok := a.mcpAuthorizeRequest(w, r)
	if !ok {
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil || sess.CsrfToken == nil {
		// No session: sign in first, then come back to this exact URL.
		http.Redirect(w, r, "/?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil || id.TeamID == 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	teamName := ""
	if team, err := a.Store.GetTeamByID(r.Context(), id.TeamID); err == nil {
		teamName = team.Name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(mcpConsentPage(client, req, teamName, *sess.CsrfToken)))
}

// McpApprove implements POST /oauth/mcp/approve: the explicit grant. The
// session's CSRF token must be echoed by the form, so only AkerDock's own
// consent page can trigger it.
func (a *API) McpApprove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	req, client, ok := a.mcpAuthorizeRequest(w, r)
	if !ok {
		return
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil || sess.CsrfToken == nil {
		writeOAuthError(w, http.StatusUnauthorized, "access_denied", "no active session")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Form.Get("csrf_token")), []byte(*sess.CsrfToken)) != 1 {
		writeOAuthError(w, http.StatusForbidden, "access_denied", "invalid CSRF token")
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil || id.TeamID == 0 {
		writeOAuthError(w, http.StatusUnauthorized, "access_denied", "no active session")
		return
	}
	// The user said no: tell the client, per OAuth.
	if r.Form.Get("approve") != "yes" {
		redirectOAuthError(w, r, req.RedirectURI, req.State, "access_denied", "the user declined")
		return
	}

	code := make([]byte, 32)
	if _, err := rand.Read(code); err != nil {
		redirectOAuthError(w, r, req.RedirectURI, req.State, "server_error", "cannot mint a code")
		return
	}
	raw := hex.EncodeToString(code)
	_ = a.Store.DeleteExpiredMcpOauthCodes(r.Context())
	if err := a.Store.CreateMcpOauthCode(r.Context(), store.CreateMcpOauthCodeParams{
		CodeHash: hashMcpToken(raw), ClientID: req.ClientID, UserID: sess.UserID, TeamID: id.TeamID,
		RedirectUri: req.RedirectURI, CodeChallenge: req.Challenge,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(mcpCodeTTL), Valid: true},
	}); err != nil {
		redirectOAuthError(w, r, req.RedirectURI, req.State, "server_error", "cannot store the code")
		return
	}
	a.recordAudit(r, id, "mcp.authorize", "team", pgtype.UUID{})
	a.Logger.Info("mcp authorization granted",
		"client", client.Name, "verified", client.Verified, "origin", client.Origin, "team_id", id.TeamID)

	target, _ := url.Parse(req.RedirectURI)
	q := target.Query()
	q.Set("code", raw)
	if req.State != "" {
		q.Set("state", req.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// mcpAuthorizeParams is one validated authorization request, carried from the
// consent screen to the approval unchanged.
type mcpAuthorizeParams struct {
	ClientID    string
	RedirectURI string
	State       string
	Challenge   string
}

// mcpAuthorizeRequest validates the OAuth parameters and resolves the client
// (CIMD or registered). Shared by the consent screen and the approval, so the
// approval can never be pushed parameters the screen would have refused.
func (a *API) mcpAuthorizeRequest(w http.ResponseWriter, r *http.Request) (mcpAuthorizeParams, mcpClient, bool) {
	if !a.mcpEnabled(r) {
		http.NotFound(w, r)
		return mcpAuthorizeParams{}, mcpClient{}, false
	}
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		values = r.Form
	}
	req := mcpAuthorizeParams{
		ClientID:    values.Get("client_id"),
		RedirectURI: values.Get("redirect_uri"),
		State:       values.Get("state"),
		Challenge:   values.Get("code_challenge"),
	}
	client, err := a.resolveMcpClient(r.Context(), req.ClientID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", err.Error())
		return req, client, false
	}
	if !allowedRedirect(client.RedirectURIs, req.RedirectURI) {
		// Never redirect to an unregistered uri — that is the open-redirect
		// rule of the whole flow.
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is not registered for this client")
		return req, client, false
	}
	if values.Get("response_type") != "code" {
		redirectOAuthError(w, r, req.RedirectURI, req.State, "unsupported_response_type", "only the authorization code flow is supported")
		return req, client, false
	}
	if req.Challenge == "" || values.Get("code_challenge_method") != "S256" {
		redirectOAuthError(w, r, req.RedirectURI, req.State, "invalid_request", "PKCE with S256 is mandatory")
		return req, client, false
	}
	if a.Sessions == nil {
		writeOAuthError(w, http.StatusConflict, "server_error", "sessions are not available on this deployment")
		return req, client, false
	}
	return req, client, true
}

// McpToken implements POST /oauth/mcp/token: the code exchange, PKCE verified.
func (a *API) McpToken(w http.ResponseWriter, r *http.Request) {
	if !a.mcpEnabled(r) {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	// Consuming the code makes a replay find nothing.
	row, err := a.Store.TakeMcpOauthCode(r.Context(), hashMcpToken(code))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	}
	if row.ClientID != r.Form.Get("client_id") || row.RedirectUri != r.Form.Get("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri does not match the code")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != row.CodeChallenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	client, err := a.resolveMcpClient(r.Context(), row.ClientID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", err.Error())
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "cannot mint a token")
		return
	}
	token := mcpTokenScheme + hex.EncodeToString(raw)
	if _, err := a.Store.CreateMcpAccessToken(r.Context(), store.CreateMcpAccessTokenParams{
		TokenHash: hashMcpToken(token), ClientID: row.ClientID, ClientName: client.Name,
		UserID: row.UserID, TeamID: row.TeamID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(mcpTokenTTL), Valid: true},
	}); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "cannot store the token")
		return
	}
	a.Logger.Info("mcp access token issued", "client", client.Name, "team_id", row.TeamID)
	writeJSONNoStore(w, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(mcpTokenTTL.Seconds()),
		"scope":        "akerdock:read",
	})
}

// validRedirectURI accepts https anywhere, http only on loopback (a local MCP
// client's callback), and refuses anything else.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	default:
		return false
	}
}

func allowedRedirect(registered []string, candidate string) bool {
	for _, uri := range registered {
		if uri == candidate {
			return true
		}
	}
	return false
}

func writeJSONNoStore(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

// redirectOAuthError returns the error to the client's redirect uri — the
// only correct way to fail once the uri is proven registered.
func redirectOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, code, description)
		return
	}
	q := target.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// mcpAPIToken resolves an API token (`akd_`) to its team for the MCP surface:
// the same prefix pre-filter and constant-time comparison as the bearer
// middleware, plus the read requirement — MCP never exposes more than a
// viewer sees.
func (a *API) mcpAPIToken(r *http.Request, token string) (int64, bool) {
	if len(token) < auth.PrefixLen {
		return 0, false
	}
	candidates, err := a.Store.GetActiveApiTokensByPrefix(r.Context(), token[:auth.PrefixLen])
	if err != nil {
		return 0, false
	}
	hash := auth.HashToken(token)
	for i := range candidates {
		if !auth.HashEqual(hash, candidates[i].TokenHash) {
			continue
		}
		match := candidates[i]
		if match.ExpiresAt.Valid && time.Now().After(match.ExpiresAt.Time) {
			return 0, false
		}
		perms := auth.EffectivePermissions(match.Permissions)
		if !auth.Has(perms, auth.PermRead) && !auth.Has(perms, auth.PermRoot) {
			return 0, false
		}
		return match.TeamID, true
	}
	return 0, false
}
