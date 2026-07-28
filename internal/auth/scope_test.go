package auth

import (
	"slices"
	"testing"
)

func ptrID(v int64) *int64 { return &v }

// Roles used across the table below, reduced to what each test asserts.
var (
	memberish = []string{"applications:read", "applications:deploy", "secrets:write", "servers:read", "audit:read"}
	reviewish = []string{"previews:read"}
	adminish  = []string{"applications:read", "applications:deploy", "members:manage", "servers:manage", "secrets:reveal", "servers:read"}
)

func base(perms ...string) Assignment {
	return Assignment{Scope: TeamScope, Permissions: perms, RoleName: "base"}
}

func onProject(id int64, perms ...string) Assignment {
	return Assignment{Scope: Scope{ProjectID: ptrID(id)}, Permissions: perms, RoleName: "scoped"}
}

func onEnvironment(project, env int64, perms ...string) Assignment {
	return Assignment{
		Scope:       Scope{ProjectID: ptrID(project), EnvironmentID: ptrID(env)},
		Permissions: perms, RoleName: "scoped",
	}
}

func inProject(id int64) Scope { return Scope{ProjectID: ptrID(id)} }

func inEnvironment(project, env int64) Scope {
	return Scope{ProjectID: ptrID(project), EnvironmentID: ptrID(env)}
}

// Inertia is a requirement, not a hope (ADR-046 §11): with no assignment, the
// base role applies in full and nothing about the old behavior changes.
func TestNoAssignmentLeavesTheBaseRoleUntouched(t *testing.T) {
	got := Resolve(base(memberish...), nil, inProject(1))
	for _, want := range memberish {
		if !slices.Contains(got, want) {
			t.Errorf("without any assignment the base role must apply in full, missing %q", want)
		}
	}
	// Including the team-only ones: the base role is held AT the team scope, so
	// nothing about it is dropped.
	if !slices.Contains(got, "audit:read") {
		t.Error("a team-level audit:read must survive: the base role is a team assignment")
	}
}

// The override is the point of the feature: a narrow scope may REDUCE rights,
// which is what makes "everything except production" expressible.
func TestNarrowScopeOverridesAndMayReduce(t *testing.T) {
	subject := base(memberish...)
	scoped := []Assignment{onProject(7, reviewish...)}

	// On project 7 the reviewer role replaces the member one.
	got := Resolve(subject, scoped, inProject(7))
	if slices.Contains(got, "applications:deploy") {
		t.Error("the scoped role must REPLACE the base one, not add to it")
	}
	if !slices.Contains(got, "previews:read") {
		t.Error("the scoped role's own permissions must apply")
	}

	// Everywhere else the base role is intact.
	elsewhere := Resolve(subject, scoped, inProject(8))
	if !slices.Contains(elsewhere, "applications:deploy") {
		t.Error("an assignment on project 7 must not narrow project 8")
	}
}

// A project assignment covers the project's environments; an environment
// assignment covers only its own.
func TestInheritanceDownTheScopeChain(t *testing.T) {
	subject := base(reviewish...)
	scoped := []Assignment{onProject(7, memberish...)}

	if !CanOn(subject, scoped, inEnvironment(7, 42), "applications:deploy") {
		t.Error("a project assignment must cover that project's environments")
	}
	if CanOn(subject, scoped, inEnvironment(9, 43), "applications:deploy") {
		t.Error("a project assignment must not reach another project's environment")
	}

	// The most specific wins: staging yes, production no.
	staging := onEnvironment(7, 42, "applications:deploy", "applications:read")
	production := onEnvironment(7, 43, "applications:read")
	scoped = []Assignment{onProject(7, "applications:read"), staging, production}

	if !CanOn(subject, scoped, inEnvironment(7, 42), "applications:deploy") {
		t.Error("deploy on staging must be granted by the environment assignment")
	}
	if CanOn(subject, scoped, inEnvironment(7, 43), "applications:deploy") {
		t.Error("production must not inherit staging's deploy — the most specific scope wins")
	}
}

// Union applies at equal scope only. Across scopes it would make the override
// meaningless, since the broader role would always win by addition.
func TestUnionAppliesAtEqualScopeOnly(t *testing.T) {
	subject := base(memberish...)
	scoped := []Assignment{
		onProject(7, "previews:read"),
		onProject(7, "logs:read"),
	}
	got := Resolve(subject, scoped, inProject(7))
	if !slices.Contains(got, "previews:read") || !slices.Contains(got, "logs:read") {
		t.Errorf("two roles at the same scope must union, got %v", got)
	}
	if slices.Contains(got, "applications:deploy") {
		t.Error("the base role must not union across scopes with the project ones")
	}
}

// Team-only permissions are never conferred by a scoped assignment: a project
// "admin" must not manage servers, members or read the team's audit trail.
func TestScopedAssignmentNeverConfersTeamOnlyPermissions(t *testing.T) {
	subject := base(reviewish...)
	scoped := []Assignment{onProject(7, adminish...)}
	got := Resolve(subject, scoped, inProject(7))

	for _, forbidden := range []string{"members:manage", "servers:manage", "secrets:reveal"} {
		if slices.Contains(got, forbidden) {
			t.Errorf("a scoped assignment must not confer team-only %q", forbidden)
		}
	}
	if !slices.Contains(got, "applications:deploy") {
		t.Error("its scoped permissions must still apply")
	}
}

// Team-read permissions are conferred team-wide, or a scoped member cannot see
// the server their application lands on — and therefore cannot deploy at all.
func TestTeamReadPermissionsAreHoistedTeamWide(t *testing.T) {
	subject := base() // `none`: an empty base role
	scoped := []Assignment{onProject(7, memberish...)}

	if !slices.Contains(Resolve(subject, scoped, TeamScope), "servers:read") {
		t.Error("servers:read must be granted team-wide by a scoped assignment")
	}
	if slices.Contains(Resolve(subject, scoped, TeamScope), "applications:deploy") {
		t.Error("a scoped deploy must NOT leak to the team scope")
	}
}

// The empty base role is what makes partitioning possible at all (ADR-046 §2).
func TestEmptyBaseRoleGrantsNothingOutsideItsScopes(t *testing.T) {
	subject := base()
	scoped := []Assignment{onProject(7, memberish...)}

	// Outside their scopes they hold no SCOPED permission. Team-read ones are
	// still there and must be: `servers:read` is what lets them see the server
	// their application lands on, and it is a property of the team, not of a
	// project. Asserting "nothing at all" would be asserting the wrong rule.
	for _, p := range Resolve(subject, scoped, inProject(9)) {
		if ClassOf(p) == ClassScoped {
			t.Errorf("a `none` member holds scoped %q outside their scopes", p)
		}
	}
	if !slices.Contains(Resolve(subject, scoped, inProject(9)), "servers:read") {
		t.Error("team-read permissions must survive outside the scoped assignment")
	}
	if !CanOn(subject, scoped, inProject(7), "applications:deploy") {
		t.Error("…and everything their scope grants inside it")
	}
}

// The classification must be exhaustive: a permission added to the catalogue
// without a class is one whose behavior under scoping nobody decided
// (rbac-matrix §3.3).
func TestEveryCataloguePermissionIsClassified(t *testing.T) {
	scoped := 0
	for name := range Catalog {
		switch ClassOf(name) {
		case ClassTeamOnly, ClassTeamRead:
		default:
			scoped++
			// A scoped classification is the default, so assert the ones that
			// must NOT be: an administrative permission silently treated as
			// scoped would be conferred by a project assignment.
			if name == "members:manage" || name == "instance:manage" || name == "audit:read" {
				t.Errorf("%q must be team-only", name)
			}
		}
	}
	if scoped == 0 {
		t.Error("no permission is scoped — the classification is inverted")
	}
	// The three classes must cover the catalogue exactly.
	if len(teamRead)+len(teamOnly) >= len(Catalog) {
		t.Errorf("team classes (%d) cover the whole catalogue (%d): nothing left to scope",
			len(teamRead)+len(teamOnly), len(Catalog))
	}
}

// ScopesCovering is what the access review renders: narrowest first, the base
// role last, so a reader sees the rule that actually applied at the top.
func TestScopesCoveringOrdersNarrowestFirst(t *testing.T) {
	subject := base(memberish...)
	scoped := []Assignment{onProject(7, reviewish...), onEnvironment(7, 42, reviewish...)}

	got := ScopesCovering(subject, scoped, inEnvironment(7, 42))
	if len(got) != 3 {
		t.Fatalf("covering scopes = %d, want environment + project + team", len(got))
	}
	if got[0].Scope.EnvironmentID == nil || got[1].Scope.ProjectID == nil || !got[2].Scope.IsTeam() {
		t.Errorf("order = %v, want environment, project, team", got)
	}
}
