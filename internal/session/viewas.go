package session

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
)

// ErrRoleNotFound is returned by SetViewAs when the requested role does not
// exist for the session's team.
var ErrRoleNotFound = errors.New("role not found")

// ErrNotAllowedToViewAs is returned when the session's own authority is not
// enough to enter the mode.
var ErrNotAllowedToViewAs = errors.New("not allowed to inspect roles")

// viewAsSystemRoles are the system roles a session may simulate. `owner` is
// legacy (ADR-038 merged it into admin) and `none` is a withdrawn value: neither
// is a view anybody needs to inspect.
var viewAsSystemRoles = map[string]store.TeamRole{
	"admin":    store.TeamRoleAdmin,
	"member":   store.TeamRoleMember,
	"reviewer": store.TeamRoleReviewer,
}

// narrowToViewAs applies the session's role-inspection mode (ADR-058): the
// permissions it really holds, intersected with the simulated role's. An
// intersection is the whole safety argument — the result is always a subset of
// what the session already held, so no path through this code can grant
// anything.
//
// Returns the (possibly unchanged) permissions and the label to show. An empty
// label means the session acts with its own authority.
func (m *Manager) narrowToViewAs(ctx context.Context, row store.GetSessionByTokenHashRow, teamID int64, perms []string) ([]string, string) {
	switch {
	case row.ViewAsCustomRoleID != nil:
		role, err := m.Store.GetCustomRoleByID(ctx, *row.ViewAsCustomRoleID)
		if err != nil || role.TeamID != teamID {
			// The role vanished mid-session, or the session moved to another
			// team while still pointing at this one's role. Granting nothing is
			// the only safe answer: silently handing the operator their real
			// powers back, under a banner still claiming "you are a reviewer",
			// is how an inspection turns into an accidental production change.
			return nil, "unknown role"
		}
		return auth.Intersect(perms, auth.ExpandGranular(role.Permissions)), role.Name
	case row.ViewAsRole != nil:
		role := *row.ViewAsRole
		return auth.Intersect(perms, auth.ExpandGranular(auth.PermissionsForRole(role))), string(role)
	default:
		return perms, ""
	}
}

// MayInspectRoles reports whether the user may enter the role-inspection mode,
// and in which team, from their REAL membership — never from the permissions a
// session currently presents, which the mode itself may already have narrowed.
//
// Inspecting roles is an administration act: it is how an admin verifies what
// they are shipping to their team. A member has nobody to inspect for, and the
// mode would only be a way to lose their own buttons.
// preferredTeamID is the team the session acts in (its current_team_id, 0 for
// none): authority is per team, so an admin of team A must not be able to
// inspect from a session pinned to team B where they are only a member.
func (m *Manager) MayInspectRoles(ctx context.Context, userID, preferredTeamID int64) (bool, int64, error) {
	membership, err := m.Store.GetTeamMembershipForUser(ctx, store.GetTeamMembershipForUserParams{
		UserID: userID, PreferredTeamID: preferredTeamID,
	})
	if err != nil {
		return false, 0, err
	}
	allowed := membership.IsRoot ||
		membership.Role == store.TeamRoleAdmin ||
		membership.Role == store.TeamRoleOwner
	return allowed, membership.TeamID, nil
}

// SetViewAs enters or leaves the role-inspection mode for one session. role is
// a system role name, customRoleUUID a custom role of the session's team;
// leaving is both empty.
//
// Authority is read from the session's REAL membership, never from the
// permissions the session currently presents — otherwise an admin who entered
// reviewer mode could no longer prove they may leave it, and would be locked
// into a degraded session until the cookie expired.
func (m *Manager) SetViewAs(ctx context.Context, userID, sessionID, preferredTeamID int64, role, customRoleUUID string) (string, error) {
	role, customRoleUUID = strings.TrimSpace(role), strings.TrimSpace(customRoleUUID)

	// LEAVING is unconditional. It restores the session to its own authority and
	// can therefore never grant anything — while gating it would strand anybody
	// whose role changed mid-inspection: an admin demoted to member while
	// simulating `reviewer` would fail the check below and stay a reviewer until
	// the cookie expired.
	if role == "" && customRoleUUID == "" {
		return "", m.Store.SetSessionViewAs(ctx, store.SetSessionViewAsParams{ID: sessionID})
	}

	allowed, teamID, err := m.MayInspectRoles(ctx, userID, preferredTeamID)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrNotAllowedToViewAs
	}

	switch {
	case customRoleUUID != "":
		var u pgtype.UUID
		if err := u.Scan(customRoleUUID); err != nil {
			return "", ErrRoleNotFound
		}
		custom, err := m.Store.GetCustomRoleByUUID(ctx, store.GetCustomRoleByUUIDParams{
			Uuid: u, TeamID: teamID,
		})
		if err != nil {
			return "", ErrRoleNotFound
		}
		if err := m.Store.SetSessionViewAs(ctx, store.SetSessionViewAsParams{
			ID: sessionID, ViewAsCustomRoleID: &custom.ID,
		}); err != nil {
			return "", err
		}
		return custom.Name, nil

	default:
		systemRole, ok := viewAsSystemRoles[strings.ToLower(role)]
		if !ok {
			return "", ErrRoleNotFound
		}
		if err := m.Store.SetSessionViewAs(ctx, store.SetSessionViewAsParams{
			ID: sessionID, ViewAsRole: &systemRole,
		}); err != nil {
			return "", err
		}
		return string(systemRole), nil
	}
}
