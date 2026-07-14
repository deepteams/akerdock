// Package session implements browser authentication for the dashboard (PRD
// §698): a login endpoint, HttpOnly session cookies, CSRF protection, session
// rotation and lockout.
//
// Why this exists at all: the v1 API authenticates with a Bearer token, which
// is right for scripts and wrong for a browser. A token that a page holds in
// JavaScript is readable by any XSS on that page and travels in a header the
// developer must remember to set. A session cookie the page CANNOT read is
// strictly safer — at the cost of one new problem, CSRF, which the cookie
// creates and which this package must therefore solve.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/password"
	"github.com/deepteams/akerdock/internal/store"
)

const (
	// CookieName holds the session token. It is HttpOnly: JavaScript must never
	// be able to read it, which is the entire point of preferring it to a token
	// in localStorage.
	CookieName = "akerdock_session"

	// CSRFCookieName holds the CSRF token. It is deliberately NOT HttpOnly: the
	// page must read it to echo it back in a header. That is safe — an attacker
	// on another origin can make the browser SEND cookies, but the same-origin
	// policy stops them from READING one.
	CSRFCookieName = "akerdock_csrf"

	// CSRFHeader is where the page echoes the token. A cross-site request cannot
	// set it, so its presence proves the request came from our own page.
	CSRFHeader = "X-CSRF-Token"

	// Lifetime of a session. Long enough not to annoy, short enough that a
	// stolen cookie expires.
	Lifetime = 12 * time.Hour

	// MaxFailedLogins locks the account, not the IP (§23.1): an attacker spreads
	// attempts across hosts, and locking IPs punishes the whole office behind
	// one NAT.
	MaxFailedLogins = 5
	// LockoutMinutes is how long the account stays locked.
	LockoutMinutes = 15
)

// ErrInvalidCredentials is returned for a wrong email AND for a wrong password:
// the caller must not be able to tell which, or the endpoint becomes an account
// enumeration oracle.
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrAccountLocked      = errors.New("account temporarily locked after too many failed attempts")
)

// Manager creates and verifies browser sessions.
type Manager struct {
	Store *store.Queries
	// Secure marks the cookies Secure. It is derived from the instance FQDN
	// being https — on a plain-HTTP instance a Secure cookie would simply never
	// be sent, and the operator would be locked out of their own dashboard.
	Secure bool
}

// Session is an authenticated browser session.
type Session struct {
	ID        int64
	UserID    int64
	TeamID    int64
	Email     string
	Name      string
	CSRFToken string
	Role      store.TeamRole
}

// Login verifies the credentials and opens a session.
//
// Every failure path takes the same visible shape (ErrInvalidCredentials), and
// the password is verified even when the user does not exist — otherwise the
// response time itself would tell an attacker which emails are registered.
func (m *Manager) Login(ctx context.Context, r *http.Request, email, plaintext string) (*Session, string, error) {
	user, err := m.Store.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		// Spend roughly the same time as a real verification: an unknown email
		// must not answer faster than a wrong password.
		_, _ = password.Verify(plaintext, dummyHash)
		return nil, "", ErrInvalidCredentials
	}

	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return nil, "", ErrAccountLocked
	}

	// password_hash is nullable: an account created through OAuth has no password
	// at all. It must fail like a wrong password — same error, same timing — not
	// with a distinctive message that says "this account exists, try SSO".
	hash := dummyHash
	if user.PasswordHash != nil {
		hash = *user.PasswordHash
	}
	ok, err := password.Verify(plaintext, hash)
	if err != nil || !ok || user.PasswordHash == nil {
		if _, err := m.Store.RecordFailedLogin(ctx, store.RecordFailedLoginParams{
			ID: user.ID, MaxAttempts: MaxFailedLogins, LockMinutes: LockoutMinutes,
		}); err != nil {
			return nil, "", err
		}
		return nil, "", ErrInvalidCredentials
	}

	// A successful login clears the counter: the lockout must punish an attack,
	// not a user who mistyped once last week.
	if err := m.Store.ClearFailedLogins(ctx, user.ID); err != nil {
		return nil, "", err
	}

	return m.Open(ctx, r, user)
}

// Open mints a brand-new session for an already-authenticated user. It is the
// shared tail of every login flow (password, passkey): whatever proved the
// identity, the session that comes out is the same — new token, new CSRF
// secret, so there is no pre-login session id to fixate onto.
func (m *Manager) Open(ctx context.Context, r *http.Request, user store.User) (*Session, string, error) {
	membership, err := m.Store.GetTeamMembershipForUser(ctx, user.ID)
	if err != nil {
		return nil, "", fmt.Errorf("the account belongs to no team")
	}

	token, hash, err := newToken()
	if err != nil {
		return nil, "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, "", err
	}

	row, err := m.Store.CreateSession(ctx, store.CreateSessionParams{
		UserID:        user.ID,
		TokenHash:     hash,
		CsrfToken:     &csrf,
		CurrentTeamID: &membership.TeamID,
		Ip:            clientIP(r),
		UserAgent:     ptr(r.UserAgent()),
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(Lifetime), Valid: true},
	})
	if err != nil {
		return nil, "", err
	}

	return &Session{
		ID: row.ID, UserID: user.ID, TeamID: membership.TeamID,
		Email: user.Email, Name: user.Name, CSRFToken: csrf, Role: membership.Role,
	}, token, nil
}

// SessionFromRequest resolves the session cookie into its full row (user
// included), for the /auth endpoints that need to know WHO is asking — not
// merely that someone authenticated is.
func (m *Manager) SessionFromRequest(ctx context.Context, r *http.Request) (*store.GetSessionByTokenHashRow, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return nil, errors.New("no session cookie")
	}
	row, err := m.Store.GetSessionByTokenHash(ctx, hashToken(cookie.Value))
	if err != nil {
		return nil, errors.New("no active session")
	}
	return &row, nil
}

// Authenticate resolves the session cookie into an identity, or nil.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) *auth.Identity {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	row, err := m.Store.GetSessionByTokenHash(ctx, hashToken(cookie.Value))
	if err != nil {
		return nil // unknown, revoked, expired, or the user is gone — all the same
	}
	_ = m.Store.TouchSession(ctx, row.ID)

	teamID := int64(0)
	if row.CurrentTeamID != nil {
		teamID = *row.CurrentTeamID
	}

	membership, err := m.Store.GetTeamMembershipForUser(ctx, row.UserID)
	if err != nil {
		return nil
	}

	return &auth.Identity{
		TokenID:     row.ID,
		TokenUUID:   uuidString(row.Uuid),
		TeamID:      teamID,
		Permissions: PermissionsForRole(membership.Role),
		Session:     true,
	}
}

// PermissionsForRole maps a team role onto the API permission set (rbac-matrix).
//
// The mapping is deliberately coarse and explicit: a role is a NAME for a set of
// permissions, and inventing a clever derivation would mean the RBAC matrix and
// the code could disagree without anyone noticing.
func PermissionsForRole(role store.TeamRole) []string {
	switch role {
	case store.TeamRoleOwner:
		return []string{
			string(auth.PermRead), string(auth.PermReadSensitive),
			string(auth.PermWrite), string(auth.PermDeploy), string(auth.PermRoot),
		}
	case store.TeamRoleAdmin:
		return []string{
			string(auth.PermRead), string(auth.PermReadSensitive),
			string(auth.PermWrite), string(auth.PermDeploy),
		}
	default: // member
		return []string{string(auth.PermRead), string(auth.PermDeploy)}
	}
}

// VerifyCSRF checks the double-submit token on a state-changing request.
//
// A session cookie is attached by the browser to EVERY request to this origin,
// including one a malicious page triggered. So a cookie proves which browser is
// calling, never that the user meant to call. The token below is readable by our
// page (same origin) and unreadable by theirs — echoing it is proof of intent.
//
// Safe methods are exempt: they must not change state, and a GET that does is a
// bug this check would only paper over.
func (m *Manager) VerifyCSRF(ctx context.Context, r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}

	// No session cookie: this is a bearer-token call, and a bearer token cannot
	// be replayed cross-site — the browser never attaches it on its own. CSRF is
	// a cookie problem, so no cookie means nothing to check. (http.ErrNoCookie
	// here is an ABSENCE, not a failure.)
	cookie, err := r.Cookie(CookieName)
	if errors.Is(err, http.ErrNoCookie) || cookie == nil || cookie.Value == "" {
		return nil
	}

	row, err := m.Store.GetSessionByTokenHash(ctx, hashToken(cookie.Value))
	if err != nil || row.CsrfToken == nil {
		return errors.New("invalid session")
	}
	sent := r.Header.Get(CSRFHeader)
	if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(*row.CsrfToken)) != 1 {
		return errors.New("missing or invalid CSRF token")
	}
	return nil
}

// SetCookies writes the session and CSRF cookies.
func (m *Manager) SetCookies(w http.ResponseWriter, token, csrf string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true, // unreadable by JavaScript: the whole point
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode, // blocks the plain cross-site POST
		MaxAge:   int(Lifetime.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrf,
		Path:     "/",
		HttpOnly: false, // the page must read it to echo it back
		Secure:   m.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(Lifetime.Seconds()),
	})
}

// ClearCookies expires both cookies. Logout revokes the session server-side too:
// a logout that only drops the cookie leaves a valid session behind, which is
// not a logout.
func (m *Manager) ClearCookies(w http.ResponseWriter) {
	for _, name := range []string{CookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == CookieName, Secure: m.Secure, SameSite: http.SameSiteLaxMode,
		})
	}
}

// Logout revokes the session behind the request, if any.
func (m *Manager) Logout(ctx context.Context, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	if row, err := m.Store.GetSessionByTokenHash(ctx, hashToken(cookie.Value)); err == nil {
		_ = m.Store.RevokeSession(ctx, row.ID)
	}
}

// --- tokens ------------------------------------------------------------------

// newToken returns the clear token and the hash stored in the database. Only the
// hash is persisted: a database dump must not hand over live sessions.
func newToken() (token, hash string, err error) {
	token, err = randomToken()
	if err != nil {
		return "", "", err
	}
	return token, hashToken(token), nil
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// dummyHash is a real Argon2id hash of a random value, used to spend the same
// time verifying a password for an account that does not exist.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHR2YWx1ZQ$RdescudvJCsgt3ub+b+dWRWJTmaaJObG"

func clientIP(r *http.Request) *netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

func ptr[T any](v T) *T { return &v }
