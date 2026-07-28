package handlers

import (
	"slices"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// The acceptance criteria of rbac-matrix §6.2, expressed against the resolution
// the handlers actually call. They are the reason ADR-046 is more than a
// document: each one is a boundary somebody will lean on.

func perms(role store.TeamRole) []string {
	return auth.ExpandGranular(session.PermissionsForRole(role))
}

func partitioned(assignments ...auth.Assignment) *auth.Identity {
	return &auth.Identity{TeamID: 1, Permissions: perms(store.TeamRoleNone), ScopedAssignments: assignments}
}

func memberOn(scope auth.Scope) auth.Assignment {
	return auth.Assignment{Scope: scope, Permissions: perms(store.TeamRoleMember), RoleName: "member"}
}

// "A member scoped to staging cannot act on production of the same project."
func TestScopedToStagingCannotTouchProduction(t *testing.T) {
	project, staging, production := int64(7), int64(41), int64(42)
	id := partitioned(memberOn(auth.Scope{ProjectID: &project, EnvironmentID: &staging}))

	if !id.CanOnScope(auth.Scope{ProjectID: &project, EnvironmentID: &staging}, auth.PermApplicationsDeploy) {
		t.Error("staging must be deployable")
	}
	if id.CanOnScope(auth.Scope{ProjectID: &project, EnvironmentID: &production}, auth.PermApplicationsDeploy) {
		t.Error("production of the same project must be refused")
	}
	if id.CanOnScope(auth.Scope{ProjectID: &project, EnvironmentID: &production}, auth.PermApplicationsRead) {
		t.Error("…and not even readable: out of scope is invisible, not merely forbidden")
	}
}

// "A role scoped to project X grants nothing on project Y, including through an
// indirect route." The indirect routes all resolve the same scope, which is why
// putting the check in the resolvers closes them together.
func TestProjectScopeDoesNotLeakSideways(t *testing.T) {
	x, y := int64(7), int64(9)
	id := partitioned(memberOn(auth.Scope{ProjectID: &x}))

	for _, perm := range []auth.Permission{
		auth.PermApplicationsRead, auth.PermDatabasesRead, auth.PermServicesRead,
		auth.PermDeploymentsRead, auth.PermBackupsRead, auth.PermPreviewsRead,
		auth.PermSecretsRead, auth.PermLogsRead, auth.PermTerminalOpen,
	} {
		if id.CanOnScope(auth.Scope{ProjectID: &y}, perm) {
			t.Errorf("%s leaked from project X to project Y", perm)
		}
	}
}

// "Team-only permissions are not conferred by a scoped assignment" — a project
// admin is not a team admin.
func TestScopedAdminIsNotATeamAdmin(t *testing.T) {
	project := int64(7)
	id := partitioned(auth.Assignment{
		Scope:       auth.Scope{ProjectID: &project},
		Permissions: perms(store.TeamRoleAdmin),
		RoleName:    "admin",
	})
	scope := auth.Scope{ProjectID: &project}

	for _, forbidden := range []auth.Permission{
		auth.PermMembersManage, auth.PermRolesManage, auth.PermTokensCreate,
		auth.PermServersManage, auth.PermKeysManage, auth.PermAuditRead,
		auth.PermTerminalRoot, "secrets:reveal", "databases:credentials",
		auth.PermProjectsCreate,
	} {
		if id.CanOnScope(scope, forbidden) {
			t.Errorf("a project-scoped admin must not hold team-only %q", forbidden)
		}
	}
	// …while everything scoped is theirs on that project.
	if !id.CanOnScope(scope, auth.PermApplicationsDelete) {
		t.Error("a project-scoped admin must fully administer that project's resources")
	}
}

// "Team-read permissions are conferred team-wide, or the member cannot deploy
// at all."
func TestScopedMemberCanStillSeeTheInfrastructure(t *testing.T) {
	project := int64(7)
	id := partitioned(memberOn(auth.Scope{ProjectID: &project}))

	for _, needed := range []auth.Permission{
		auth.PermServersRead, auth.PermKeysRead, auth.PermSourcesRead, auth.PermTeamRead,
	} {
		if !id.CanOnScope(auth.TeamScope, needed) {
			t.Errorf("%s must be held team-wide, or deploying is impossible", needed)
		}
	}
	// The sensitive halves stay behind their own permissions (INV-003).
	if id.CanOnScope(auth.TeamScope, "keys:reveal") {
		t.Error("keys:reveal must not ride along with keys:read")
	}
}

// "A `none` member with no assignment gets an empty dashboard rather than an
// error" — nothing is granted, and nothing about that is exceptional.
func TestNoneMemberWithoutAssignmentHoldsNothing(t *testing.T) {
	id := partitioned()
	for _, perm := range []auth.Permission{
		auth.PermApplicationsRead, auth.PermProjectsRead, auth.PermServersRead, auth.PermTeamRead,
	} {
		if id.CanOnScope(auth.TeamScope, perm) {
			t.Errorf("a `none` member with no assignment must hold nothing, got %q", perm)
		}
	}
	if id.Scoped() {
		t.Error("no assignment means not scoped: the resolution must short-circuit")
	}
}

// The system roles' own sets must stay what rbac-matrix §2 says, since every
// scoped assignment is built from them.
func TestSystemRolesUnderScoping(t *testing.T) {
	if len(perms(store.TeamRoleNone)) != 0 {
		t.Errorf("`none` must be empty, got %v", perms(store.TeamRoleNone))
	}
	if !slices.Contains(perms(store.TeamRoleMember), string(auth.PermProjectsCreate)) {
		t.Error("member keeps projects:create — splitting it off must not remove it from the role")
	}
	if slices.Contains(perms(store.TeamRoleReviewer), string(auth.PermProjectsCreate)) {
		t.Error("reviewer must not gain projects:create")
	}
}
