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
	"io"
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
	// ErrPasswordLoginDisabled is returned when the instance is in SSO-only mode
	// (password_login_disabled) and a non-root user tries to sign in by password.
	ErrPasswordLoginDisabled = errors.New("password login is disabled on this instance — sign in with SSO")
)

// Manager creates and verifies browser sessions.
type Manager struct {
	Store Store
	// Secure marks the cookies Secure. It is derived from the instance FQDN
	// being https — on a plain-HTTP instance a Secure cookie would simply never
	// be sent, and the operator would be locked out of their own dashboard.
	Secure bool
}

var randomReader = rand.Reader

// Session is an authenticated browser session.
type Session struct {
	ID        int64
	UserID    int64
	TeamID    int64
	Email     string
	Name      string
	CSRFToken string
	Role      store.TeamRole
	// MFAPending is true when the instance requires MFA but this user has no
	// confirmed factor yet: the session may only enroll one (forced enrollment).
	MFAPending bool
}

// Login verifies the credentials and opens a session — unless the account
// has 2FA, in which case it returns ErrMFARequired and the returned string is
// a CHALLENGE token, not a session token: the login finishes in
// TOTP.VerifyLogin, and no session exists until it does.
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

	// SSO-only mode (§10.2): the instance can disable password login entirely.
	// The instance root is exempt — an escape hatch so a misconfigured OIDC can
	// never lock the platform administrator out of their own instance.
	if settings, err := m.Store.GetInstanceSettings(ctx); err == nil && settings.PasswordLoginDisabled && !user.IsRoot {
		return nil, "", ErrPasswordLoginDisabled
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

	// A confirmed TOTP factor turns this into step one of two: the password
	// buys a short-lived challenge, never a session (PRD §10.2). An
	// unconfirmed factor guards nothing and changes nothing.
	//
	// The failed-login counter is deliberately NOT cleared here: failed TOTP
	// attempts count into it, and clearing it on the password step would let
	// whoever holds the password reset their code-guessing budget by simply
	// logging in again. The counter clears when the login COMPLETES —
	// TOTP.VerifyLogin does it after a valid code.
	if factor, err := m.Store.GetMfaFactorForUser(ctx, user.ID); err == nil && factor.ConfirmedAt.Valid {
		challenge, err := m.CreateChallenge(ctx, user.ID)
		if err != nil {
			return nil, "", err
		}
		return nil, challenge, ErrMFARequired
	}

	// A successful login clears the counter: the lockout must punish an attack,
	// not a user who mistyped once last week.
	if err := m.Store.ClearFailedLogins(ctx, user.ID); err != nil {
		return nil, "", err
	}

	// A bare password is a single factor: forced MFA enrolment may still apply.
	return m.Open(ctx, r, user, false)
}

// Open mints a brand-new session for an already-authenticated user. It is the
// shared tail of every login flow (password, passkey, federated): whatever
// proved the identity, the session that comes out is the same — new token, new
// CSRF secret, so there is no pre-login session id to fixate onto.
//
// mfaSatisfied says the login method already provided multi-factor assurance,
// so forced MFA enrolment does not apply: a passkey (user verification is
// REQUIRED — possession + biometric/PIN) and a federated login (the IdP owns
// the second factor, §10.2) both pass it, as does a login that just cleared the
// TOTP challenge. Only a bare password sets it false.
func (m *Manager) Open(ctx context.Context, r *http.Request, user store.User, mfaSatisfied bool) (*Session, string, error) {
	// The session opens on the team the user last acted in (PRD §37): a member of
	// several teams who switched and signed out expects to come back where they
	// left off, not on their oldest team. The preference is only that — the query
	// still requires a live membership, so a team the user was removed from since
	// falls back to one they really hold.
	preferred := int64(0)
	if user.LastTeamID != nil {
		preferred = *user.LastTeamID
	}
	membership, err := m.Store.GetTeamMembershipForUser(ctx, store.GetTeamMembershipForUserParams{
		UserID: user.ID, PreferredTeamID: preferred,
	})
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

	// Forced MFA enrollment (ISO A.8.5): if the instance requires MFA and this
	// user has no MFA-grade factor, the session opens PENDING — usable only to
	// enroll one, blocked on the API until they do. A factor is a confirmed TOTP
	// secret OR a passkey (which does user verification, so it is multi-factor on
	// its own). A login that already carried its own assurance — passkey,
	// federated, or a cleared TOTP challenge — is never pending.
	pending := false
	if !mfaSatisfied {
		if settings, err := m.Store.GetInstanceSettings(ctx); err == nil && settings.MfaRequired {
			pending = !m.hasMFAFactor(ctx, user.ID)
		}
	}

	row, err := m.Store.CreateSession(ctx, store.CreateSessionParams{
		UserID:        user.ID,
		TokenHash:     hash,
		CsrfToken:     &csrf,
		CurrentTeamID: &membership.TeamID,
		Ip:            clientIP(r),
		UserAgent:     ptr(r.UserAgent()),
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(Lifetime), Valid: true},
		MfaPending:    pending,
	})
	if err != nil {
		return nil, "", err
	}

	return &Session{
		ID: row.ID, UserID: user.ID, TeamID: membership.TeamID,
		Email: user.Email, Name: user.Name, CSRFToken: csrf, Role: membership.Role,
		MFAPending: pending,
	}, token, nil
}

// hasMFAFactor reports whether the user already holds an MFA-grade factor: a
// confirmed TOTP secret, or at least one passkey (user verification is required
// on passkeys, so a single one is possession + inherence — multi-factor).
func (m *Manager) hasMFAFactor(ctx context.Context, userID int64) bool {
	if factor, err := m.Store.GetMfaFactorForUser(ctx, userID); err == nil && factor.ConfirmedAt.Valid {
		return true
	}
	if n, err := m.Store.CountPasskeysForUser(ctx, userID); err == nil && n > 0 {
		return true
	}
	return false
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

	// The team the session asked to act in. It is resolved THROUGH the
	// membership, never trusted on its own: role, permissions, team id and team
	// uuid must all come from the same row, or a session could end up carrying
	// the permissions it holds in one team while addressing another team's data
	// (INV-001). A session pointing at a team the user is no longer a member of
	// — removed, or the team soft-deleted — gets their oldest membership back
	// instead, which is a demotion, never an escalation.
	preferred := int64(0)
	if row.CurrentTeamID != nil {
		preferred = *row.CurrentTeamID
	}
	membership, err := m.Store.GetTeamMembershipForUser(ctx, store.GetTeamMembershipForUserParams{
		UserID: row.UserID, PreferredTeamID: preferred,
	})
	if err != nil {
		return nil
	}
	teamID := membership.TeamID

	// A custom role (ADR-038) overrides the system role: its stored granular
	// permissions become the identity's, expanded like any set. The permissions
	// were validated (⊆ composer, never instance:*) and closed at write time.
	granular := PermissionsForRole(membership.Role)
	if len(membership.CustomPermissions) > 0 {
		granular = membership.CustomPermissions
	}
	perms := auth.ExpandGranular(granular)
	// The instance root (users.is_root) is the platform administrator, outside the
	// team-role model (ADR-038 §1). Its SESSION carries the coarse `root` wildcard
	// so it can act across every team (e.g. list all teams) — never a token, which
	// is team-bound and stops at the team boundary (rbac-matrix §3.5).
	if membership.IsRoot {
		perms = append(perms, string(auth.PermRoot))
	}

	return &auth.Identity{
		TokenID:      row.ID,
		TokenUUID:    uuidString(row.Uuid),
		TeamID:       teamID,
		TeamUUID:     uuidString(membership.TeamUuid),
		Permissions:  perms,
		Session:      true,
		InstanceRoot: membership.IsRoot,
		MFAPending:   row.MfaPending,
		UserID:       &row.UserID,
	}
}

// TeamMembership is one team the user may act in, as offered by the dashboard's
// team switcher.
type TeamMembership struct {
	TeamID   int64
	UUID     string
	Name     string
	Role     store.TeamRole
	Personal bool
}

// Teams lists the teams a user is a member of (PRD §37). Memberships only:
// the instance root sees every team through GET /teams, but may still only ACT
// in the teams somebody added them to.
func (m *Manager) Teams(ctx context.Context, userID int64) ([]TeamMembership, error) {
	rows, err := m.Store.ListTeamMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]TeamMembership, 0, len(rows))
	for _, row := range rows {
		out = append(out, TeamMembership{
			TeamID: row.TeamID, UUID: uuidString(row.TeamUuid),
			Name: row.TeamName, Role: row.Role, Personal: row.Personal,
		})
	}
	return out, nil
}

// ErrNotAMember is returned when a session asks to act in a team the user does
// not belong to. It is deliberately the same answer for a team that does not
// exist and one that exists but is somebody else's: switching must not become
// an oracle telling which team UUIDs are real (INV-002).
var ErrNotAMember = errors.New("team not found")

// SwitchTeam moves a session into another of the user's teams and remembers the
// choice for their next login.
//
// The membership is re-read here rather than taken from the caller: this is the
// one place that widens what a live session can reach, so the check that the
// user really belongs to the target team has to happen at the moment of the
// move, against the database — not against anything the browser sent.
//
// The identity is NOT re-derived from this call: the next request re-reads the
// session row and resolves permissions from the new team's membership, so the
// switch takes effect exactly where every other authorization decision is made.
func (m *Manager) SwitchTeam(ctx context.Context, userID int64, sessionID int64, teamUUID string) (TeamMembership, error) {
	teams, err := m.Store.ListTeamMembershipsForUser(ctx, userID)
	if err != nil {
		return TeamMembership{}, err
	}
	for _, row := range teams {
		if uuidString(row.TeamUuid) != teamUUID {
			continue
		}
		team := row.TeamID
		if _, err := m.Store.SetSessionCurrentTeam(ctx, store.SetSessionCurrentTeamParams{
			ID: sessionID, CurrentTeamID: &team,
		}); err != nil {
			return TeamMembership{}, err
		}
		// Best-effort: failing to remember the choice must not fail the switch —
		// the session already moved, and the worst case is the next login opening
		// on another team.
		_ = m.Store.SetUserLastTeam(ctx, store.SetUserLastTeamParams{ID: userID, LastTeamID: &team})
		return TeamMembership{
			TeamID: row.TeamID, UUID: uuidString(row.TeamUuid),
			Name: row.TeamName, Role: row.Role, Personal: row.Personal,
		}, nil
	}
	return TeamMembership{}, ErrNotAMember
}

// PermissionsForRole is auth.PermissionsForRole, kept here because the session
// package is where callers historically found it. The table itself lives in
// internal/auth: bearer authentication needs it too, and internal/auth cannot
// import this package (it is the other way round).
func PermissionsForRole(role store.TeamRole) []string { return auth.PermissionsForRole(role) }

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

// CookiesWouldBeDropped says whether the session cookies this manager mints
// can never come back on requests like this one: a Secure cookie set over
// plain HTTP is stored by the browser and then silently never sent — the
// login "succeeds" into a permanent 401 with nothing in the logs. The check
// recognizes TLS terminated by a fronting proxy through X-Forwarded-Proto.
//
// Loopback is exempt on purpose: browsers treat http://localhost as a secure
// context and DO deliver Secure cookies there, and an SSH tunnel to localhost
// is THE emergency door when the TLS front of the instance is down — a rule
// that locked that door would turn a proxy outage into a control-plane
// lockout.
func (m *Manager) CookiesWouldBeDropped(r *http.Request) bool {
	if !m.Secure || r.TLS != nil {
		return false
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return false
	}
	return !isLoopbackHost(r.Host)
}

// isLoopbackHost says whether the requested host is the local machine —
// localhost, *.localhost, 127.0.0.0/8 or ::1, with or without a port.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	if _, err := io.ReadFull(randomReader, raw); err != nil {
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
