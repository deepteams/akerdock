package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
)

// idWithScopes builds a PARTITIONED caller: base role `none` (no team-wide
// permission) plus scoped assignments. That combination is the whole point —
// with a base role that still grants everything, an assignment is an exception
// and takes nothing away elsewhere (ADR-046 §2).
func idWithScopes(assignments ...auth.Assignment) *auth.Identity {
	return &auth.Identity{
		TeamID:            1,
		Permissions:       nil, // `none`
		Required:          "applications:deploy",
		ScopedAssignments: assignments,
	}
}

// idWithBaseAndScopes is a member who kept their team-wide role and holds one
// exception on top of it.
func idWithBaseAndScopes(base []string, assignments ...auth.Assignment) *auth.Identity {
	return &auth.Identity{
		TeamID:            1,
		Permissions:       base,
		Required:          "applications:deploy",
		ScopedAssignments: assignments,
	}
}

func projectScope(id int64) auth.Scope { return auth.Scope{ProjectID: &id} }

// Inertia is a requirement of ADR-046 §11: an instance that never partitioned
// anything must behave exactly as it did. The check is not "it usually passes"
// — it is that the scope path is not even entered.
func TestUnscopedCallerSkipsTheScopeCheckEntirely(t *testing.T) {
	a, _ := flowAPI(t)
	r := httptest.NewRequest("GET", "/applications/x", nil)

	id := &auth.Identity{TeamID: 1, Permissions: []string{"applications:read"}, Required: "applications:read"}
	env := int64(42)
	if !a.allowedAtScope(r, id, &env, "") {
		t.Error("a caller with no assignment must be allowed without any resolution")
	}
	// Even a caller holding nothing: `require` already refused those, and the
	// scope check must not become a second, different gate.
	empty := &auth.Identity{TeamID: 1, Required: "applications:deploy"}
	if !a.allowedAtScope(r, empty, &env, "") {
		t.Error("the scope check is inert without assignments, whatever the permissions")
	}
}

// The resolver's question is the operation's own permission, re-evaluated at
// the resource's scope (ADR-046 §6): holding it somewhere is not holding it
// here.
func TestScopedCallerIsRefusedOutsideTheirScope(t *testing.T) {
	id := idWithScopes(auth.Assignment{
		Scope:       projectScope(7),
		Permissions: []string{"applications:read", "applications:deploy"},
		RoleName:    "member",
	})

	if !id.CanOnScope(projectScope(7), "applications:deploy") {
		t.Error("inside their project, the scoped role applies")
	}
	if id.CanOnScope(projectScope(9), "applications:deploy") {
		t.Error("outside it, holding the permission team-wide must NOT be enough")
	}
}

// A narrow scope may reduce: the base role does not come back through the side
// door when the scoped role grants less.
func TestScopedRoleReplacesTheBaseRole(t *testing.T) {
	id := idWithBaseAndScopes([]string{"applications:read", "applications:deploy"}, auth.Assignment{
		Scope:       projectScope(7),
		Permissions: []string{"previews:read"},
		RoleName:    "reviewer",
	})
	if id.CanOnScope(projectScope(7), "applications:deploy") {
		t.Error("a reviewer assignment on project 7 must remove the team-wide deploy there")
	}
	if !id.CanOnScope(projectScope(8), "applications:deploy") {
		t.Error("…and leave every other project untouched")
	}
}

// An instance root is outside the team model and reaches everything
// (rbac-matrix §3.9); scoping must not become a way to lock the platform
// administrator out of an instance they are responsible for.
func TestInstanceRootIsNotNarrowedByScopes(t *testing.T) {
	id := idWithScopes(auth.Assignment{Scope: projectScope(7), Permissions: []string{"previews:read"}})
	id.Permissions = []string{string(auth.PermRoot)}
	if !id.CanOnScope(projectScope(9), "applications:deploy") {
		t.Error("a root identity must not be narrowed by a scoped assignment")
	}
}

// Someone scoped to one environment must still see the project it lives in, or
// the tree that leads to their own environment is unreachable.
func TestEnvironmentScopeConfersPathVisibility(t *testing.T) {
	project, env := int64(7), int64(42)
	id := idWithScopes(auth.Assignment{
		Scope:       auth.Scope{ProjectID: &project, EnvironmentID: &env},
		Permissions: []string{"applications:read", "applications:deploy"},
		RoleName:    "member",
	})
	id.Required = "projects:read"

	if !id.CanOnScope(projectScope(7), "projects:read") {
		t.Error("the parent project must be visible, or the environment cannot be navigated to")
	}
	if id.CanOnScope(projectScope(7), "applications:deploy") {
		t.Error("path visibility must open a path, never a resource")
	}
	if id.CanOnScope(projectScope(9), "projects:read") {
		t.Error("an unrelated project must stay invisible")
	}
}

// The trap this feature lives or dies by (ADR-046 §2): an assignment is an
// EXCEPTION to the base role. Give somebody `member` on the team and `member`
// on one project, and they still reach every other project — nothing was
// partitioned. Only a `none` base role turns assignments into a boundary.
func TestAssignmentsAloneDoNotPartition(t *testing.T) {
	full := []string{"applications:read", "applications:deploy"}
	kept := idWithBaseAndScopes(full, auth.Assignment{
		Scope:       projectScope(7),
		Permissions: full,
		RoleName:    "member",
	})
	if !kept.CanOnScope(projectScope(9), "applications:deploy") {
		t.Error("a member who kept their team role still reaches other projects — that is the model")
	}

	partitioned := idWithScopes(auth.Assignment{
		Scope:       projectScope(7),
		Permissions: full,
		RoleName:    "member",
	})
	if partitioned.CanOnScope(projectScope(9), "applications:deploy") {
		t.Error("with a `none` base role, everything outside the assignment must be refused")
	}
	if !partitioned.CanOnScope(projectScope(7), "applications:deploy") {
		t.Error("…and everything inside it granted")
	}
}
