package auth

import (
	"slices"
	"testing"
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
		PermTeamRead, PermTeamManage, PermMembersRead, PermInvitationsManage,
		PermTokensRead, PermTokensCreate, PermTokensRevoke,
	} {
		if _, ok := Catalog[string(p)]; !ok {
			t.Errorf("constant %q is not in Catalog", p)
		}
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
