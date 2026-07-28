package handlers

import (
	"slices"
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// The review must agree with the enforcement, or it is a screen that reassures
// about a rule it does not share. Both read the same resolution: a member's
// capabilities are exactly what their permissions grant.
func TestAccessCapabilitiesAgreeWithTheResolution(t *testing.T) {
	kinds := []string{"application", "database", "service", "project", "environment"}
	roles := []store.TeamRole{store.TeamRoleAdmin, store.TeamRoleMember, store.TeamRoleReviewer}

	for _, role := range roles {
		perms := auth.ExpandGranular(session.PermissionsForRole(role))
		for _, kind := range kinds {
			for _, c := range capabilitiesFor(kind) {
				granted := auth.Has(perms, c.perm)
				listed := slices.Contains(capabilitiesOf(perms, kind), c.label)
				if granted != listed {
					t.Errorf("%s on %s: capability %q listed=%v but permission %q granted=%v",
						role, kind, c.label, listed, c.perm, granted)
				}
			}
		}
	}
}

// A reviewer sees PR previews and nothing else (ADR-038). The review must show
// that as an absence, not as a row claiming access to a database.
func TestReviewerHoldsNoCapabilityOnResources(t *testing.T) {
	perms := auth.ExpandGranular(session.PermissionsForRole(store.TeamRoleReviewer))
	for _, kind := range []string{"application", "database", "service", "project", "environment"} {
		if caps := capabilitiesOf(perms, kind); len(caps) != 0 {
			t.Errorf("reviewer on %s = %v, want no capability", kind, caps)
		}
	}
}

// The asymmetry of rbac-matrix §2 must survive into the review: a member writes
// configuration and cannot read a secret back (INV-003). A view that reported
// "secrets" for every member would make the distinction invisible exactly where
// someone looks for it.
func TestMemberHasNoSecretCapability(t *testing.T) {
	perms := auth.ExpandGranular(session.PermissionsForRole(store.TeamRoleMember))

	appCaps := capabilitiesOf(perms, "application")
	if !slices.Contains(appCaps, "deploy") {
		t.Errorf("member on an application = %v, want it to include deploy", appCaps)
	}
	if slices.Contains(appCaps, "secrets") {
		t.Errorf("member on an application = %v, want NO secrets capability (INV-003)", appCaps)
	}
	if slices.Contains(capabilitiesOf(perms, "database"), "secrets") {
		t.Error("member must not be reported as able to read database credentials")
	}

	admin := auth.ExpandGranular(session.PermissionsForRole(store.TeamRoleAdmin))
	if !slices.Contains(capabilitiesOf(admin, "application"), "secrets") {
		t.Error("admin does hold secrets:reveal and must be reported as such")
	}
}

// An instance root reaches every team (rbac-matrix §3.9). Omitting it would
// make the view lie by exactly the account an auditor asks about first.
func TestInstanceRootIsResolvedAsReachingEverything(t *testing.T) {
	root := accessMember{role: store.TeamRoleReviewer, isRoot: true}
	perms := memberPermissions(root)
	for _, kind := range []string{"application", "database", "project"} {
		caps := capabilitiesOf(perms, kind)
		if len(caps) != len(capabilitiesFor(kind)) {
			t.Errorf("instance root on %s = %v, want every capability", kind, caps)
		}
	}
	if got := memberRoleLabel(root); got != "instance root" {
		t.Errorf("role label = %q, want it to say instance root rather than its team role", got)
	}
}

// A custom role overrides the system one — the same rule the session applies at
// authentication (session.go). If the review resolved it differently, the two
// would disagree precisely on the roles a team took the trouble to compose.
func TestCustomRoleOverridesTheSystemRoleInTheReview(t *testing.T) {
	name := "deployer"
	m := accessMember{
		role:        store.TeamRoleAdmin,
		customName:  &name,
		customPerms: []string{string(auth.PermApplicationsRead), string(auth.PermApplicationsDeploy)},
	}
	caps := capabilitiesOf(memberPermissions(m), "application")
	if !slices.Contains(caps, "deploy") || !slices.Contains(caps, "view") {
		t.Errorf("custom role capabilities = %v, want view and deploy", caps)
	}
	if slices.Contains(caps, "secrets") || slices.Contains(caps, "delete") {
		t.Errorf("custom role capabilities = %v, want the admin base NOT to leak through", caps)
	}
	if got := memberRoleLabel(m); got != "deployer" {
		t.Errorf("role label = %q, want the custom role's name", got)
	}
}
