package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/store"
)

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

	sum := sha256.Sum256([]byte(strings.TrimSpace(body.Token)))
	inv, err := a.Store.AcceptInvitation(r.Context(), hex.EncodeToString(sum[:]))
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
