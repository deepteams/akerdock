// CLI login (ADR-031): a poll + confirmation-code + PKCE flow that opens no
// local port. /auth/cli/start creates a request, the browser approves it on
// the panel (/auth/cli/approve, session + CSRF), and the CLI exchanges its
// verifier for a normal akd_ token (/auth/cli/token). All out of contract,
// mounted next to /auth. Only hashes are stored; the verifier never reaches
// the browser.

package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	cliCodeTTL       = 10 * time.Minute
	cliPollInterval  = 2 // seconds
	cliTokenTTL      = 30 * 24 * time.Hour
	userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no ambiguous 0/O/1/I
	userCodeLength   = 8
	defaultCliPerms  = "read,write"
)

func hashRequestID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func randomUserCode() (string, error) {
	raw := make([]byte, userCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	b := make([]byte, userCodeLength)
	for i, c := range raw {
		b[i] = userCodeAlphabet[int(c)%len(userCodeAlphabet)]
	}
	return string(b), nil
}

// CliAuthStart implements POST /auth/cli/start (unauthenticated, per-IP
// limited): registers a login request bound to the CLI's PKCE challenge.
func (a *API) CliAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Challenge string `json:"challenge"`
		Name      string `json:"name"`
		Scopes    string `json:"scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Challenge == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "challenge is required")
		return
	}
	requestID, err := randomHex(16)
	if err != nil {
		a.internalError(w, r, "cli start", err)
		return
	}
	userCode, err := randomUserCode()
	if err != nil {
		a.internalError(w, r, "cli start", err)
		return
	}
	scopes := body.Scopes
	if scopes == "" {
		scopes = defaultCliPerms
	}
	var name *string
	if body.Name != "" {
		name = &body.Name
	}
	if _, err := a.Store.CreateCliAuthCode(r.Context(), store.CreateCliAuthCodeParams{
		RequestIDHash: hashRequestID(requestID),
		Challenge:     body.Challenge,
		UserCode:      userCode,
		ClientName:    name,
		ClientIp:      clientAddr(r),
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(cliCodeTTL), Valid: true},
	}); err != nil {
		a.internalError(w, r, "cli start", err)
		return
	}
	// The requested scopes ride back to the CLI and are shown on the consent
	// page; approval narrows them against the session's own permissions.
	_ = scopes
	verifyURL := ""
	if st, err := a.Settings.Get(r.Context()); err == nil && st.Fqdn != nil && *st.Fqdn != "" {
		verifyURL = "https://" + *st.Fqdn + "/cli/authorize?request_id=" + requestID
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"request_id": requestID,
		"user_code":  userCode,
		"verify_url": verifyURL,
		"scopes":     scopes,
		"interval":   cliPollInterval,
		"expires_in": int(cliCodeTTL.Seconds()),
	})
}

// CliAuthRequest implements GET /auth/cli/request?request_id= (session): the
// consent page reads the pending request's public metadata to render it.
func (a *API) CliAuthRequest(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil || a.Sessions.Authenticate(r.Context(), r) == nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "sign in first")
		return
	}
	requestID := r.URL.Query().Get("request_id")
	code, err := a.Store.GetCliAuthCodeByRequestHash(r.Context(), hashRequestID(requestID))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "unknown or expired request")
		return
	}
	name := ""
	if code.ClientName != nil {
		name = *code.ClientName
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"user_code": code.UserCode,
		"name":      name,
		"status":    code.Status,
	})
}

// CliAuthApprove implements POST /auth/cli/approve (session + CSRF): the
// maintainer's explicit consent. Permissions are narrowed to the session's.
func (a *API) CliAuthApprove(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "sessions unavailable")
		return
	}
	id := a.Sessions.Authenticate(r.Context(), r)
	if id == nil {
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeUnauthorized, "sign in first")
		return
	}
	if err := a.Sessions.VerifyCSRF(r.Context(), r); err != nil {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "invalid CSRF token")
		return
	}
	var body struct {
		RequestID   string   `json:"request_id"`
		TeamUUID    string   `json:"team_uuid"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.RequestID == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "request_id is required")
		return
	}
	team, ok := a.resolveTeam(w, r, id, body.TeamUUID)
	if !ok {
		return
	}
	perms := body.Permissions
	if len(perms) == 0 {
		perms = strings.Split(defaultCliPerms, ",")
	}
	for _, p := range perms {
		if !validPermission(p) {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "unknown permission "+p)
			return
		}
		if !auth.Has(id.Permissions, auth.Permission(p)) {
			httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
				"cannot grant the "+p+" permission you do not hold")
			return
		}
	}
	sess, err := a.Sessions.SessionFromRequest(r.Context(), r)
	if err != nil {
		a.internalError(w, r, "cli approve", err)
		return
	}
	n, err := a.Store.ApproveCliAuthCode(r.Context(), store.ApproveCliAuthCodeParams{
		RequestIDHash: hashRequestID(body.RequestID),
		UserID:        &sess.UserID,
		TeamID:        &team.ID,
		Permissions:   perms,
	})
	if err != nil {
		a.internalError(w, r, "cli approve", err)
		return
	}
	if n == 0 {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "request expired or already handled")
		return
	}
	a.recordAudit(r, id, "cli.authorize", "team", team.Uuid)
	w.WriteHeader(http.StatusNoContent)
}

// CliAuthToken implements POST /auth/cli/token (unauthenticated, per-IP
// limited): the CLI's poll. Pending until approved; then the verifier is
// checked against the challenge and a normal akd_ token is minted.
func (a *API) CliAuthToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
		Verifier  string `json:"verifier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.RequestID == "" || body.Verifier == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "request_id and verifier are required")
		return
	}
	code, err := a.Store.GetCliAuthCodeByRequestHash(r.Context(), hashRequestID(body.RequestID))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "unknown or expired request")
		return
	}
	if code.Status != "approved" {
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	// PKCE: possession of the request id is not enough — only the holder of
	// the verifier that produced the challenge gets the token. The CLI sends
	// challenge = base64url(sha256(verifier)); compare in the same encoding.
	sum := sha256.Sum256([]byte(body.Verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != code.Challenge {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "verifier does not match the challenge")
		return
	}
	if code.TeamID == nil {
		httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "request has no team")
		return
	}
	// Consume atomically: a second poll after this wins nothing.
	consumed, err := a.Store.ConsumeCliAuthCode(r.Context(), hashRequestID(body.RequestID))
	if err != nil {
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	team, err := a.Store.GetTeamByID(r.Context(), *consumed.TeamID)
	if err != nil {
		a.internalError(w, r, "cli token", err)
		return
	}

	value, prefix, hash, err := auth.NewToken()
	if err != nil {
		a.internalError(w, r, "cli token", err)
		return
	}
	name := "cli"
	if consumed.ClientName != nil && *consumed.ClientName != "" {
		name = "cli — " + *consumed.ClientName
	}
	created, err := a.Store.CreateApiToken(r.Context(), store.CreateApiTokenParams{
		TeamID:      *consumed.TeamID,
		Name:        name,
		TokenPrefix: prefix,
		TokenHash:   hash,
		Permissions: consumed.Permissions,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(cliTokenTTL), Valid: true},
		// The user who approved this login in the browser owns the token: it is
		// capped by their permissions from now on, and an access grant issued to
		// them is spendable through it (ADR-045 §5).
		CreatedBy: consumed.UserID,
	})
	if err != nil {
		a.internalError(w, r, "cli token", err)
		return
	}
	a.Audit.System(r.Context(), consumed.TeamID, "token.create", "api_token", created.Uuid, store.AuditResultSuccess)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"status":    "approved",
		"token":     value,
		"team_uuid": uuidString(team.Uuid),
	})
}
