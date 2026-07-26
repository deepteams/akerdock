package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/notify"
	"github.com/deepteams/akerdock/internal/store"
)

// defaultInvitationHours is the 7-day validity of the OpenAPI contract.
const defaultInvitationHours = 168

// invitationStatus derives the exposed status from the row timestamps.
func invitationStatus(inv store.Invitation) api.InvitationStatus {
	switch {
	case inv.AcceptedAt.Valid:
		return "accepted"
	case inv.RevokedAt.Valid:
		return "revoked"
	case inv.ExpiresAt.Valid && time.Now().After(inv.ExpiresAt.Time):
		return "expired"
	default:
		return "pending"
	}
}

// invitationToAPI renders an invitation. inviteURL is only set at creation
// (the link is a credential: it is hashed at rest and never returned again).
func invitationToAPI(inv store.Invitation, inviteURL *string) api.Invitation {
	return api.Invitation{
		Uuid:      ptr(uuidString(inv.Uuid)),
		Email:     openapi_types.Email(inv.Email),
		Role:      api.InvitationRole(inv.Role),
		Status:    ptr(invitationStatus(inv)),
		InviteUrl: inviteURL,
		ExpiresAt: inv.ExpiresAt.Time.UTC(),
		CreatedAt: timePtr(inv.CreatedAt),
		InvitedBy: nil,
	}
}

// ListTeamInvitations implements GET /teams/{team_uuid}/invitations
// (permission: read).
func (a *API) ListTeamInvitations(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.ListTeamInvitationsParams) {
	id, ok := a.require(w, r, auth.PermMembersRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListInvitationsPage(r.Context(), store.ListInvitationsPageParams{
		TeamID: team.ID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list invitations", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(i store.Invitation) int64 { return i.ID })

	data := make([]api.Invitation, 0, len(rows))
	for _, inv := range rows {
		data = append(data, invitationToAPI(inv, nil))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Invitation `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}{data, cursor})
}

// CreateTeamInvitation implements POST /teams/{team_uuid}/invitations
// (permission: write). Without transactional email configured, the invite
// link is returned in the response for manual delivery.
func (a *API) CreateTeamInvitation(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, params api.CreateTeamInvitationParams) {
	id, ok := a.require(w, r, auth.PermInvitationsManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var body api.InvitationCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	email := strings.TrimSpace(string(body.Email))
	if _, err := mail.ParseAddress(email); err != nil || strings.ContainsAny(email, " <>") {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("email"), Code: ptr("invalid"), Message: "invalid email address"}})
		return
	}
	role := store.TeamRoleMember
	if body.Role != nil && *body.Role == api.InvitationCreateRoleAdmin {
		role = store.TeamRoleAdmin
	}
	hours := defaultInvitationHours
	if body.ExpiresInHours != nil {
		if *body.ExpiresInHours < 1 || *body.ExpiresInHours > 720 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("expires_in_hours"), Code: ptr("out_of_range"), Message: "expires_in_hours must be between 1 and 720"}})
			return
		}
		hours = *body.ExpiresInHours
	}

	// The invite link is a credential: only its SHA-256 hash is stored
	// (§23.2), and the clear value is returned exactly once.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		a.internalError(w, r, "create invitation", err)
		return
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	inv, err := a.Store.CreateInvitation(r.Context(), store.CreateInvitationParams{
		TeamID: team.ID, Email: email, Role: role,
		TokenHash: hex.EncodeToString(sum[:]),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Duration(hours) * time.Hour), Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "an active invitation already exists for this email in this team")
			return
		}
		a.internalError(w, r, "create invitation", err)
		return
	}

	inviteURL := ptr("/invitations/accept?token=" + token)
	if settings, err := a.Settings.Get(r.Context()); err == nil && settings.Fqdn != nil && *settings.Fqdn != "" {
		inviteURL = ptr("https://" + *settings.Fqdn + "/invitations/accept?token=" + token)
	}

	// The mail is an ADDITION, never a replacement: the link stays in the
	// response (§23.2). An instance with no relay must still be able to invite
	// someone, and swallowing the link when the relay is misconfigured would
	// leave an invitation nobody receives and no way to hand it over.
	a.mailInvitation(r, email, *inviteURL)

	a.recordAudit(r, id, "invitation.create", "invitation", inv.Uuid)
	httpapi.WriteJSON(w, http.StatusCreated, invitationToAPI(inv, inviteURL))
}

// RevokeTeamInvitation implements DELETE
// /teams/{team_uuid}/invitations/{invitation_uuid} (permission: write): the
// link becomes immediately invalid.
func (a *API) RevokeTeamInvitation(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, invitationUuid api.InvitationUuid) {
	id, ok := a.require(w, r, auth.PermInvitationsManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(invitationUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "invitation not found")
		return
	}
	rows, err := a.Store.RevokeInvitation(r.Context(), store.RevokeInvitationParams{Uuid: u, TeamID: team.ID})
	if err != nil {
		a.internalError(w, r, "revoke invitation", err)
		return
	}
	if rows == 0 {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "invitation not found")
		return
	}
	a.recordAudit(r, id, "invitation.revoke", "invitation", u)
	w.WriteHeader(http.StatusNoContent)
}

// ResendTeamInvitation implements POST
// /teams/{team_uuid}/invitations/{invitation_uuid}/resend (permission: write):
// it rotates the link token (invalidating the previous link), pushes the expiry
// out, re-sends the email when a relay is configured, and returns the fresh link
// once — only its hash is stored, exactly like creation.
func (a *API) ResendTeamInvitation(w http.ResponseWriter, r *http.Request, teamUuid api.TeamUuid, invitationUuid api.InvitationUuid) {
	id, ok := a.require(w, r, auth.PermInvitationsManage)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUuid)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(invitationUuid); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "invitation not found")
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		a.internalError(w, r, "resend invitation", err)
		return
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	inv, err := a.Store.RotateInvitation(r.Context(), store.RotateInvitationParams{
		Uuid: u, TeamID: team.ID,
		TokenHash: hex.EncodeToString(sum[:]),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(defaultInvitationHours * time.Hour), Valid: true},
	})
	if err != nil {
		// Only a still-pending invitation can be regenerated (an accepted or
		// revoked one matches no row) — that is a 404, not a server error.
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "no pending invitation to regenerate")
			return
		}
		a.internalError(w, r, "resend invitation", err)
		return
	}

	inviteURL := ptr("/invitations/accept?token=" + token)
	if settings, err := a.Settings.Get(r.Context()); err == nil && settings.Fqdn != nil && *settings.Fqdn != "" {
		inviteURL = ptr("https://" + *settings.Fqdn + "/invitations/accept?token=" + token)
	}

	// Same contract as creation: the mail is an addition, the link always comes
	// back in the response (§23.2).
	a.mailInvitation(r, inv.Email, *inviteURL)

	a.recordAudit(r, id, "invitation.regenerate", "invitation", inv.Uuid)
	httpapi.WriteJSON(w, http.StatusOK, invitationToAPI(inv, inviteURL))
}

// mailInvitation sends the invite link through the instance's transactional
// email, if one is configured (§14.2). A failure is logged, never fatal: the
// invitation exists and its link is in the response — refusing the whole call
// because a relay hiccuped would destroy something that already worked.
func (a *API) mailInvitation(r *http.Request, email, link string) {
	cfg, ok := a.transactionalEmail(r)
	if !ok {
		return
	}
	// The recipient is the invitee, not the operator: the instance relay is a
	// transport, and every message it carries names its own audience.
	c := cfg.Config
	switch {
	case c.SMTP != nil:
		smtp := *c.SMTP
		smtp.To = []string{email}
		c.SMTP = &smtp
	case c.Resend != nil:
		resend := *c.Resend
		resend.To = []string{email}
		c.Resend = &resend
	default:
		return
	}
	event := notify.Event{Type: "team.invitation.v1", Resource: link}
	if err := notify.New().Send(r.Context(), cfg.Kind, c, event); err != nil {
		a.Logger.Warn("the invitation mail could not be sent — the link is still in the API response",
			"email", email, "error", err)
	}
}
