// Package auth implements the bearer-token authentication and permission
// model of the public API (§10.3): team-scoped tokens with granular
// permissions, SHA-256 hashed with an identification prefix.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

var randomReader = rand.Reader

// Permission is a granular API token permission.
type Permission string

// The closed permission set of §10.3.
const (
	PermRead          Permission = "read"
	PermReadSensitive Permission = "read:sensitive"
	PermWrite         Permission = "write"
	PermDeploy        Permission = "deploy"
	PermRoot          Permission = "root"
)

// AllPermissions is the closed set accepted by the api_tokens CHECK.
var AllPermissions = []Permission{PermRead, PermReadSensitive, PermWrite, PermDeploy, PermRoot}

// Has evaluates the permission hierarchy of the OpenAPI preamble: root
// includes everything; write, read:sensitive and deploy include read.
func Has(granted []string, required Permission) bool {
	for _, g := range granted {
		if g == string(PermRoot) || g == string(required) {
			return true
		}
		if required == PermRead {
			switch Permission(g) {
			case PermWrite, PermReadSensitive, PermDeploy:
				return true
			}
		}
	}
	return false
}

// Identity is the authenticated caller attached to the request context.
type Identity struct {
	TokenID     int64
	TokenUUID   string
	TeamID      int64
	TeamUUID    string
	Permissions []string
	// Session is true when the caller authenticated with the dashboard
	// session cookie rather than a bearer token. The api_enabled gate
	// applies to tokens only (PRD §10.3 — it governs the public API), so
	// the dashboard keeps working, and the setting can be flipped from it.
	Session bool
	// InstanceRoot is true only for a session of the instance root user
	// (users.is_root, established at bootstrap) — the platform administrator,
	// OUTSIDE the team-role model (rbac-matrix §3.5). It gates instance-wide
	// settings (/system/*). A team owner/admin's team-scoped `root` permission
	// does NOT set it, and API tokens are team-bound so never carry it.
	InstanceRoot bool
	// Display is a human label for the actor recorded in the audit trail (§23.4):
	// an API token's name, so an audit reader sees which token acted rather than
	// only its uuid. Empty for sessions (the auth events carry the email).
	Display string
	// MFAPending is true for a session opened under forced MFA enrollment
	// (instance mfa_required, user without a confirmed factor): it may only
	// enroll a factor — every other operation is refused until it does.
	MFAPending bool

	// UserID is the acting human, when there is one — the session's user, or a
	// token's creator once its permissions have been capped by theirs (§4.2).
	UserID *int64
}

// IsRoot reports whether the token carries the root permission.
func (id *Identity) IsRoot() bool { return Has(id.Permissions, PermRoot) }

// CanAccessTeam applies team isolation (INV-001/INV-002): a token only sees
// its own team, a root token sees every team.
func (id *Identity) CanAccessTeam(teamID int64) bool {
	return id.IsRoot() || id.TeamID == teamID
}

type ctxKey struct{}

// WithIdentity attaches the authenticated identity to the context.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the authenticated identity, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok
}

// Token format: "akd_" + 48 hex characters (24 random bytes). The prefix
// stored for identification is "akd_" plus the first 6 characters (§23.2).
const (
	tokenScheme  = "akd_"
	tokenRandLen = 24
	PrefixLen    = len(tokenScheme) + 6
)

// NewToken generates a token value, its identification prefix and its
// SHA-256 hash. The clear value is returned exactly once (§10.3).
func NewToken() (token, prefix, hash string, err error) {
	raw := make([]byte, tokenRandLen)
	if _, err := io.ReadFull(randomReader, raw); err != nil {
		return "", "", "", fmt.Errorf("auth: token generation: %w", err)
	}
	token = tokenScheme + hex.EncodeToString(raw)
	return token, token[:PrefixLen], HashToken(token), nil
}

// HashToken returns the hex SHA-256 of a token value.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SplitBearer extracts the token from an Authorization header value and
// validates its shape. It returns "" when the header is not a well-formed
// AkerDock bearer token.
func SplitBearer(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenScheme) || len(token) < PrefixLen {
		return ""
	}
	return token
}

// HashEqual compares two hex hashes in constant time.
func HashEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
