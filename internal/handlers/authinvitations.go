package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/password"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// invitationTokenHash is how a link token is looked up: the clear value is
// never stored, exactly like a credential (§23.2).
func invitationTokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// InvitationInfo implements POST /auth/invitations/lookup — what the landing
// page of an invitation link needs before anyone signs in: which team, which
// address, and whether that address already has an account here.
//
// It is unauthenticated by necessity: the invitee has no account yet, which is
// the whole point. What protects it is the token — 32 bytes of entropy, hashed
// at rest, single-use, expiring. Without it this endpoint answers nothing, and
// with it the caller is by construction the person the link was mailed to.
//
// A POST for a read, deliberately: the token is a credential, and a credential
// in a URL path is a credential in every access log between here and the
// browser. The body keeps it out of them.
//
// `account_exists` decides which of two screens the dashboard shows: sign in to
// accept, or create an account. Getting that wrong is what makes an invitation
// feel broken — a stranger sent to a login form for an account nobody created.
func (a *API) InvitationInfo(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Token) == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "a token is required")
		return
	}
	invitation, err := a.Store.GetPendingInvitationByTokenHash(r.Context(), invitationTokenHash(body.Token))
	if err != nil {
		// Unknown, already used, revoked or expired — indistinguishable on
		// purpose, as in the redemption path below.
		httpapi.WriteError(w, r, http.StatusGone, "invitation_invalid",
			"this invitation link is invalid or has expired")
		return
	}

	// Whether signing in is even an option: on an SSO-only instance the answer
	// is no, and the page must say so instead of offering a password field
	// that the server would refuse.
	settings, err := a.Settings.Get(r.Context())
	if err != nil {
		a.internalError(w, r, "instance settings", err)
		return
	}
	_, err = a.Store.GetUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(invitation.Email)))
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"email":                   invitation.Email,
		"team_name":               invitation.TeamName,
		"role":                    string(invitation.Role),
		"expires_at":              invitation.ExpiresAt.Time.UTC(),
		"account_exists":          err == nil,
		"password_login_disabled": settings.PasswordLoginDisabled,
	})
}

// SignUpFromInvitation implements POST /auth/invitations/signup: the invitee
// creates their account and joins the team, in one step, from the link.
//
// This is the path that was missing. An invitation used to require an account
// that already existed, and the only way to obtain one without an admin was the
// first SSO login — so on an instance with no OAuth provider configured, an
// invitation was a dead end: the link asked the invitee to sign in, and nothing
// anywhere let them sign up.
//
// Three things make creating an account here safe:
//
//   - the EMAIL comes from the invitation, never from the request body. The
//     invitee cannot choose which address they register — the admin already did;
//   - the invitation is claimed ATOMICALLY (single-use guard in SQL), so a link
//     forwarded to two people creates at most one account;
//   - the password goes through the instance policy, like every other password.
func (a *API) SignUpFromInvitation(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	// A session that could never come back is worse than a refused signup: the
	// account would exist with no way to reach it.
	if a.refuseUndeliverableSession(w, r) {
		return
	}
	var body struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Token) == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "a token is required")
		return
	}

	// The link first: a dead link is the answer the invitee needs, and telling
	// them their password is too short before telling them the invitation
	// expired a week ago sends them round a loop they cannot win.
	invitation, err := a.Store.GetPendingInvitationByTokenHash(r.Context(), invitationTokenHash(body.Token))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusGone, "invitation_invalid",
			"this invitation link is invalid or has expired")
		return
	}

	settings, err := a.Settings.Get(r.Context())
	if err != nil {
		a.internalError(w, r, "instance settings", err)
		return
	}
	// SSO-only instance: a password account created here would be a hole in
	// exactly the policy that setting exists to enforce.
	if settings.PasswordLoginDisabled {
		httpapi.WriteError(w, r, http.StatusForbidden, "password_login_disabled",
			"password login is disabled on this instance — accept this invitation by signing in with SSO")
		return
	}
	if err := password.Validate(body.Password); err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("password"), Code: ptr("weak"), Message: err.Error(),
		}})
		return
	}

	email := strings.ToLower(strings.TrimSpace(invitation.Email))
	if _, err := a.Store.GetUserByEmail(r.Context(), email); err == nil {
		// The address already has an account: joining is then a redemption, not
		// a signup, and it must go through a real login — otherwise this
		// endpoint would let a link holder set a password on somebody's account.
		httpapi.WriteError(w, r, http.StatusConflict, "account_exists",
			"an account already exists for this address — sign in to accept the invitation")
		return
	}

	hash, err := password.Hash(body.Password)
	if err != nil {
		a.internalError(w, r, "hash password", err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = email
	}

	// Claim the invitation FIRST: it is the single-use guard, and a claim that
	// fails here must leave no account behind.
	claim, err := a.Store.AcceptInvitation(r.Context(), invitationTokenHash(body.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusGone, "invitation_invalid",
				"this invitation link is invalid or has expired")
			return
		}
		a.internalError(w, r, "accept invitation", err)
		return
	}

	user, err := a.Store.CreateUser(r.Context(), store.CreateUserParams{
		Email: email, Name: name, PasswordHash: &hash,
	})
	if err != nil {
		a.internalError(w, r, "create user", err)
		return
	}
	if err := a.Store.AddTeamMember(r.Context(), store.AddTeamMemberParams{
		TeamID: claim.TeamID, UserID: user.ID, Role: claim.Role, CustomRoleID: claim.CustomRoleID,
	}); err != nil {
		a.internalError(w, r, "add team member", err)
		return
	}

	// A password is a single factor: forced MFA enrolment applies here as it
	// does to any password login, and Open is what decides that.
	sess, token, err := a.Sessions.Open(r.Context(), r, user, false)
	if err != nil {
		a.internalError(w, r, "open session", err)
		return
	}
	a.Sessions.SetCookies(w, token, sess.CSRFToken)
	teamID := sess.TeamID
	a.auditAuth(r, "auth.invitation.signup", store.AuditResultSuccess, user.ID, email, &teamID)
	a.Logger.Info("account created from invitation", "email", email, "team_id", teamID)

	httpapi.WriteJSON(w, http.StatusCreated, map[string]any{
		"email":      sess.Email,
		"name":       sess.Name,
		"role":       string(sess.Role),
		"csrf_token": sess.CSRFToken,
		"expires_at": time.Now().Add(session.Lifetime).UTC(),
	})
}

// AcceptInvitation implements POST /auth/invitations/accept — a session endpoint
// (outside the v1 contract, like the rest of /auth): the invitee, already signed
// in, redeems an invitation link to join its team.
//
// Two guards make this safe against a leaked link:
//   - the claim is atomic and single-use (AcceptInvitation matches only a still
//     pending, unexpired invitation and stamps accepted_at in the same UPDATE);
//   - the invitation email MUST match the signed-in user's email — a link is not
//     a bearer credential for someone else's identity.
func (a *API) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	if a.Sessions == nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found")
		return
	}
	sess, ok := a.sessionUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "a token is required")
		return
	}

	inv, err := a.Store.AcceptInvitation(r.Context(), invitationTokenHash(body.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, already used, revoked or expired — all indistinguishable on
			// purpose, so the endpoint is not an invitation-probing oracle.
			httpapi.WriteError(w, r, http.StatusGone, "invitation_invalid", "this invitation link is invalid or has expired")
			return
		}
		a.internalError(w, r, "accept invitation", err)
		return
	}

	// The link must belong to THIS user: it was issued to a specific email.
	if !strings.EqualFold(strings.TrimSpace(inv.Email), strings.TrimSpace(sess.Email)) {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden, "this invitation was issued to a different email")
		return
	}

	if err := a.Store.AddTeamMember(r.Context(), store.AddTeamMemberParams{
		TeamID: inv.TeamID, UserID: sess.UserID, Role: inv.Role, CustomRoleID: inv.CustomRoleID,
	}); err != nil {
		a.internalError(w, r, "add team member", err)
		return
	}

	team, err := a.Store.GetTeamByID(r.Context(), inv.TeamID)
	if err != nil {
		a.internalError(w, r, "load team", err)
		return
	}
	a.recordAudit(r, sessionIdentity(sess), "invitation.accept", "team", team.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"team_uuid": uuidString(team.Uuid)})
}
