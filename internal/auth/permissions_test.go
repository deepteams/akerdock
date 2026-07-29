package auth

import (
	"slices"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func TestCatalogSoclesAreValid(t *testing.T) {
	valid := map[Permission]bool{
		PermRead: true, PermReadSensitive: true, PermWrite: true, PermDeploy: true, PermRoot: true,
	}
	for name, socle := range Catalog {
		if !valid[socle] {
			t.Errorf("permission %q has invalid socle %q", name, socle)
		}
		if _, _, ok := splitPerm(name); !ok {
			t.Errorf("permission %q is not a valid domaine:action", name)
		}
	}
}

func TestPermissionsForMembershipDistinguishesEmptyCustomRole(t *testing.T) {
	if got := PermissionsForMembership(store.TeamRoleMember, true, []string{}); len(got) != 0 {
		t.Fatalf("empty custom role fell back to member permissions: %v", got)
	}
	if got := PermissionsForMembership(store.TeamRoleMember, false, nil); len(got) == 0 {
		t.Fatal("membership without a custom role lost its system-role permissions")
	}
}

func TestClosureAddsDomainRead(t *testing.T) {
	got := Closure([]string{"applications:deploy"})
	if !slices.Contains(got, "applications:read") {
		t.Errorf("closure of applications:deploy = %v, want it to include applications:read", got)
	}
	// secrets:reveal (read:sensitive socle) still implies secrets:read.
	if got := Closure([]string{"secrets:reveal"}); !slices.Contains(got, "secrets:read") {
		t.Errorf("closure of secrets:reveal = %v, want secrets:read", got)
	}
}

func TestClosureCrossDomainExtras(t *testing.T) {
	cases := map[string][]string{
		"environments:deploy": {"resources:read", "environments:read"},
		"invitations:manage":  {"members:read"},
		"config:apply":        {"config:export"},
		"terminal:root":       {"terminal:open"},
		"registries:manage":   {"sources:read"},
	}
	for perm, wants := range cases {
		got := Closure([]string{perm})
		for _, w := range wants {
			if !slices.Contains(got, w) {
				t.Errorf("closure of %q = %v, want it to include %q", perm, got, w)
			}
		}
	}
}

func TestClosureReadHasNoPrereq(t *testing.T) {
	if got := Closure([]string{"applications:read"}); len(got) != 1 || got[0] != "applications:read" {
		t.Errorf("closure of a pure read = %v, want just itself", got)
	}
}

func TestProjectScopesRead(t *testing.T) {
	got := ProjectScopes([]Permission{PermRead})
	if !slices.Contains(got, "applications:read") || !slices.Contains(got, "previews:read") {
		t.Errorf("read scope must project onto the :read permissions, got %v", got)
	}
	// A read-only token holds no write/deploy/reveal/root permission.
	for _, forbidden := range []string{"applications:update", "applications:deploy", "secrets:reveal", "instance:manage"} {
		if slices.Contains(got, forbidden) {
			t.Errorf("read scope leaked %q", forbidden)
		}
	}
}

func TestProjectScopesWriteIsClosed(t *testing.T) {
	got := ProjectScopes([]Permission{PermWrite})
	// write projects onto the :manage/:update/... perms AND (via closure) their
	// :read prerequisites — even though :read's own socle is `read`.
	if !slices.Contains(got, "applications:update") || !slices.Contains(got, "applications:read") {
		t.Errorf("write scope must include the write perms and their read prereqs, got %v", got)
	}
	if slices.Contains(got, "instance:manage") {
		t.Error("write scope must not grant instance:manage (root)")
	}
}

func TestProjectScopesRootIsEverything(t *testing.T) {
	got := ProjectScopes([]Permission{PermRoot})
	if len(got) < len(Catalog) {
		t.Errorf("root scope must project onto the whole catalogue, got %d of %d", len(got), len(Catalog))
	}
	if !slices.Contains(got, "instance:encryption") {
		t.Error("root scope must include instance:encryption")
	}
}

func TestGranularConstantsInCatalog(t *testing.T) {
	// Every wired granular constant must be a real catalogue permission.
	for _, p := range []Permission{
		PermApplicationsRead, PermApplicationsCreate, PermApplicationsUpdate,
		PermApplicationsDelete, PermApplicationsDeploy, PermApplicationsLifecycle,
		PermApplicationsExec, PermResourcesAdopt,
		PermDatabasesRead, PermDatabasesCreate, PermDatabasesUpdate,
		PermDatabasesDelete, PermDatabasesLifecycle,
		PermServicesRead, PermServicesManage, PermServicesDeploy,
		PermSecretsRead, PermSecretsWrite,
		PermServersRead, PermServersManage, PermServersMaintain, PermServersProxy,
		PermResourcesRead,
		PermProjectsRead, PermProjectsManage,
		PermEnvironmentsRead, PermEnvironmentsManage,
		PermTeamRead, PermTeamManage, PermMembersRead, PermMembersManage,
		PermRolesRead, PermRolesManage, PermInvitationsManage,
		PermTokensRead, PermTokensCreate, PermTokensRevoke,
		PermPreviewsRead, PermPreviewsManage,
		PermDeploymentsRead, PermDeploymentsCancel,
		PermBackupsRead, PermBackupsManage, PermBackupsRestore,
		PermLogsRead, PermMetricsRead, PermJobsManage,
		PermNotificationsRead, PermNotificationsManage,
		PermCloudRead, PermCloudManage,
		PermCertificatesRead, PermCertificatesRenew, PermStoragesManage,
		PermKeysRead, PermKeysManage,
		PermSourcesRead, PermSourcesManage, PermRegistriesManage,
		PermTerminalOpen, PermTerminalRoot, PermPortForwardsOpen,
		PermExternalEndpointsRead, PermExternalEndpointsManage,
		PermAuditRead, PermUptimeRead, PermUptimeManage,
		PermInstanceManage, PermInstanceAudit, PermInstanceEncryption,
	} {
		if _, ok := Catalog[string(p)]; !ok {
			t.Errorf("constant %q is not in Catalog", p)
		}
	}
}

func TestValidateCustomPermissions(t *testing.T) {
	// An admin composes: holds every non-instance permission.
	admin := TeamAdminPermissions()

	// Happy path: a write permission is accepted and closed under its prereq.
	got, err := ValidateCustomPermissions([]string{"applications:deploy"}, admin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(got, "applications:read") {
		t.Errorf("closure missing prerequisite, got %v", got)
	}

	// Unknown permission is rejected.
	if _, err := ValidateCustomPermissions([]string{"nope:read"}, admin); err == nil {
		t.Error("unknown permission should be rejected")
	}

	// Instance-scoped permission can never be granted, even by an admin.
	if _, err := ValidateCustomPermissions([]string{"instance:manage"}, admin); err == nil {
		t.Error("instance-scoped permission must be rejected")
	}

	// Anti-elevation: a composer who only holds read cannot mint a write role —
	// and the prerequisite closure is checked, not just the raw input.
	reader := EffectivePermissions([]string{string(PermRead)})
	if _, err := ValidateCustomPermissions([]string{"applications:read"}, reader); err != nil {
		t.Errorf("a reader may grant a read permission: %v", err)
	}
	if _, err := ValidateCustomPermissions([]string{"applications:deploy"}, reader); err == nil {
		t.Error("a reader must not be able to grant a deploy permission")
	}
}

func TestSplitPerm(t *testing.T) {
	for _, ok := range []string{"applications:read", "a:b"} {
		if _, _, valid := splitPerm(ok); !valid {
			t.Errorf("%q should be a valid permission", ok)
		}
	}
	for _, bad := range []string{"noaction", ":read", "domain:", "a:b:c", ""} {
		if _, _, valid := splitPerm(bad); valid {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// TestTunnelPermissionIsItsOwn locks the ADR-045 §3 prerequisite: opening a
// tunnel must be grantable separately from opening a shell, and declaring an
// external endpoint must be grantable separately from using one. A regression
// here would silently re-merge powers the RBAC matrix keeps apart.
func TestTunnelPermissionIsItsOwn(t *testing.T) {
	if Catalog[string(PermPortForwardsOpen)] != PermWrite {
		t.Errorf("port-forwards:open should project onto the write socle")
	}
	// Holding the tunnel permission must not confer the shell, nor the reverse.
	tunnelOnly := Closure([]string{string(PermPortForwardsOpen)})
	if slices.Contains(tunnelOnly, string(PermTerminalOpen)) {
		t.Error("port-forwards:open must not imply terminal:open")
	}
	shellOnly := Closure([]string{string(PermTerminalOpen)})
	if slices.Contains(shellOnly, string(PermPortForwardsOpen)) {
		t.Error("terminal:open must not imply port-forwards:open")
	}
	// Declaring an endpoint is an admin act; using one is not. Neither implies
	// the other, so a member can hold the second without the first.
	useOnly := Closure([]string{string(PermExternalEndpointsRead)})
	if slices.Contains(useOnly, string(PermExternalEndpointsManage)) {
		t.Error("external-endpoints:read must not imply :manage")
	}
	// A team admin holds all four; the sets are derived from the catalogue, so
	// this also proves the new entries reached it.
	admin := TeamAdminPermissions()
	for _, p := range []Permission{
		PermPortForwardsOpen, PermExternalEndpointsRead, PermExternalEndpointsManage,
	} {
		if !slices.Contains(admin, string(p)) {
			t.Errorf("a team admin should hold %q", p)
		}
	}
}
