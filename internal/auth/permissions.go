package auth

import (
	"fmt"
	"sort"
)

// Granular permission catalogue (ADR-038, rbac-matrix §2): the real unit of
// authorization is a `domaine:action` permission. Each one maps to a coarse
// "socle" ({read, read:sensitive, write, deploy, root}) — the historical token
// scope model (§10.3) — which is how API tokens (that only carry coarse scopes)
// are projected onto the granular set.
//
// This file is the source-of-truth catalogue in code; a test cross-checks that
// every operation's OpenAPI x-required-permission is one of these names.

// Granular permission constants. Defined as they are wired into handlers,
// domain by domain (ADR-038 migration), so a typo is a compile error rather than
// a silent authorization hole. Every constant must be a key of Catalog
// (enforced by TestGranularConstantsInCatalog).
const (
	PermApplicationsRead      Permission = "applications:read"
	PermApplicationsCreate    Permission = "applications:create"
	PermApplicationsUpdate    Permission = "applications:update"
	PermApplicationsDelete    Permission = "applications:delete"
	PermApplicationsDeploy    Permission = "applications:deploy"
	PermApplicationsLifecycle Permission = "applications:lifecycle"
	PermApplicationsExec      Permission = "applications:exec"
	PermResourcesAdopt        Permission = "resources:adopt"

	PermDatabasesRead      Permission = "databases:read"
	PermDatabasesCreate    Permission = "databases:create"
	PermDatabasesUpdate    Permission = "databases:update"
	PermDatabasesDelete    Permission = "databases:delete"
	PermDatabasesLifecycle Permission = "databases:lifecycle"

	PermServicesRead   Permission = "services:read"
	PermServicesManage Permission = "services:manage"
	PermServicesDeploy Permission = "services:deploy"

	PermSecretsRead  Permission = "secrets:read"
	PermSecretsWrite Permission = "secrets:write"

	PermServersRead     Permission = "servers:read"
	PermServersManage   Permission = "servers:manage"
	PermServersMaintain Permission = "servers:maintain"
	PermServersProxy    Permission = "servers:proxy"

	PermResourcesRead Permission = "resources:read"

	PermProjectsRead   Permission = "projects:read"
	PermProjectsManage Permission = "projects:manage"

	PermEnvironmentsRead   Permission = "environments:read"
	PermEnvironmentsManage Permission = "environments:manage"

	PermTeamRead   Permission = "team:read"
	PermTeamManage Permission = "team:manage"

	PermMembersRead      Permission = "members:read"
	PermMembersManage    Permission = "members:manage"
	PermRolesRead        Permission = "roles:read"
	PermRolesManage      Permission = "roles:manage"
	PermInvitationsManage Permission = "invitations:manage"

	PermTokensRead   Permission = "tokens:read"
	PermTokensCreate Permission = "tokens:create"
	PermTokensRevoke Permission = "tokens:revoke"

	PermPreviewsRead   Permission = "previews:read"
	PermPreviewsManage Permission = "previews:manage"

	PermDeploymentsRead   Permission = "deployments:read"
	PermDeploymentsCancel Permission = "deployments:cancel"

	PermBackupsRead    Permission = "backups:read"
	PermBackupsManage  Permission = "backups:manage"
	PermBackupsRestore Permission = "backups:restore"

	PermLogsRead    Permission = "logs:read"
	PermMetricsRead Permission = "metrics:read"
	PermJobsManage  Permission = "jobs:manage"

	PermNotificationsRead   Permission = "notifications:read"
	PermNotificationsManage Permission = "notifications:manage"

	PermCloudRead   Permission = "cloud:read"
	PermCloudManage Permission = "cloud:manage"

	PermCertificatesRead  Permission = "certificates:read"
	PermCertificatesRenew Permission = "certificates:renew"

	PermStoragesManage Permission = "storages:manage"

	PermKeysRead   Permission = "keys:read"
	PermKeysManage Permission = "keys:manage"

	PermSourcesRead    Permission = "sources:read"
	PermSourcesManage  Permission = "sources:manage"
	PermRegistriesManage Permission = "registries:manage"

	PermTerminalOpen Permission = "terminal:open"
	PermTerminalRoot Permission = "terminal:root"

	PermAuditRead    Permission = "audit:read"
	PermUptimeRead   Permission = "uptime:read"
	PermUptimeManage Permission = "uptime:manage"

	// Instance-scoped permissions (root d'instance uniquement, hors modèle de
	// team). Enforcement passes through requireInstanceRoot; these name the
	// operations' x-required-permission for the contract/rbac-matrix.
	PermInstanceManage     Permission = "instance:manage"
	PermInstanceAudit      Permission = "instance:audit"
	PermInstanceEncryption Permission = "instance:encryption"
)

// Catalog maps every granular permission to its coarse socle.
var Catalog = map[string]Permission{
	// Team & access
	"team:read": PermRead, "team:manage": PermWrite,
	"members:read": PermRead, "members:manage": PermWrite,
	"invitations:manage": PermWrite,
	"roles:read":         PermRead, "roles:manage": PermWrite,
	"tokens:read": PermRead, "tokens:create": PermWrite, "tokens:revoke": PermWrite,
	// Projects & environments
	"projects:read": PermRead, "projects:manage": PermWrite,
	"environments:read": PermRead, "environments:manage": PermWrite, "environments:deploy": PermDeploy,
	"resources:read": PermRead, "resources:adopt": PermWrite,
	// Applications
	"applications:read": PermRead, "applications:create": PermWrite,
	"applications:update": PermWrite, "applications:delete": PermWrite,
	"applications:deploy": PermDeploy, "applications:lifecycle": PermDeploy, "applications:exec": PermDeploy,
	// Databases
	"databases:read": PermRead, "databases:create": PermWrite,
	"databases:update": PermWrite, "databases:delete": PermWrite,
	"databases:lifecycle": PermDeploy, "databases:credentials": PermReadSensitive,
	// Services (compose)
	"services:read": PermRead, "services:manage": PermWrite, "services:deploy": PermDeploy,
	// Secrets / env vars
	"secrets:read": PermRead, "secrets:reveal": PermReadSensitive, "secrets:write": PermWrite,
	// Servers & proxy
	"servers:read": PermRead, "servers:manage": PermWrite,
	"servers:maintain": PermWrite, "servers:proxy": PermWrite,
	// SSH keys
	"keys:read": PermRead, "keys:reveal": PermReadSensitive, "keys:manage": PermWrite,
	// Certificates
	"certificates:read": PermRead, "certificates:renew": PermWrite,
	// Git sources & registries
	"sources:read": PermRead, "sources:manage": PermWrite, "registries:manage": PermWrite,
	// Cloud & storage
	"cloud:read": PermRead, "cloud:manage": PermWrite, "storages:manage": PermWrite,
	// Backups
	"backups:read": PermRead, "backups:manage": PermWrite, "backups:restore": PermWrite,
	// Deployments & jobs
	"deployments:read": PermRead, "deployments:cancel": PermDeploy, "jobs:manage": PermWrite,
	// Previews (previews:read added by ADR-038 for the reviewer role)
	"previews:read": PermRead, "previews:manage": PermWrite,
	// Templates
	"templates:manage": PermWrite,
	// Terminal
	"terminal:open": PermWrite, "terminal:root": PermWrite,
	// Observability
	"logs:read": PermRead, "logs:manage": PermWrite,
	"metrics:read": PermRead, "audit:read": PermRead,
	"uptime:read": PermRead, "uptime:manage": PermWrite,
	"notifications:read": PermRead, "notifications:manage": PermWrite,
	// Config as code
	"config:export": PermRead, "config:apply": PermWrite,
	// Instance (root d'instance uniquement — hors modèle de team, ADR-038 §1)
	"instance:manage": PermRoot, "instance:audit": PermRoot, "instance:encryption": PermRoot,
}

// extraPrerequisites are the cross-domain dependencies that the generic
// "mutation implies the domain's :read" rule (see Prerequisites) cannot derive.
var extraPrerequisites = map[string][]string{
	"invitations:manage":  {"members:read"},
	"config:apply":        {"config:export"},
	"environments:deploy": {"resources:read", "environments:read"},
	"terminal:root":       {"terminal:open"},
	"registries:manage":   {"sources:read"},
	"storages:manage":     {"applications:read"},
	"jobs:manage":         {"deployments:read"},
}

// Prerequisites returns the permissions that `perm` directly implies: the
// domain's own `:read` for any non-read action (a role that can act on a domain
// must be able to see it), plus the cross-domain extras. Not transitive — use
// Closure for that.
func Prerequisites(perm string) []string {
	var out []string
	if dom, action, ok := splitPerm(perm); ok && action != "read" {
		if _, exists := Catalog[dom+":read"]; exists {
			out = append(out, dom+":read")
		}
	}
	out = append(out, extraPrerequisites[perm]...)
	return out
}

// Closure returns perms plus the transitive closure of their prerequisites,
// deduplicated and sorted. A role/custom role is always stored/evaluated closed,
// so an action is never granted without the reads it needs (ADR-038 §3).
func Closure(perms []string) []string {
	seen := map[string]bool{}
	var walk func(p string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, dep := range Prerequisites(p) {
			walk(dep)
		}
	}
	for _, p := range perms {
		walk(p)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ProjectScopes maps an API token's coarse scopes (§10.3) onto the granular
// permissions it holds: every catalogue permission whose socle is one of the
// token's scopes. `root` covers everything (it already does in Has). The result
// is closed under prerequisites.
func ProjectScopes(scopes []Permission) []string {
	held := map[Permission]bool{}
	for _, s := range scopes {
		held[s] = true
	}
	var out []string
	if held[PermRoot] {
		for name := range Catalog {
			out = append(out, name)
		}
		return Closure(out)
	}
	for name, socle := range Catalog {
		if held[socle] {
			out = append(out, name)
		}
	}
	return Closure(out)
}

// TeamAdminPermissions is every catalogue permission EXCEPT the instance-scoped
// ones (socle root) — the granular set of the team `admin` role (ADR-038): full
// control of the team and its resources, never instance settings. Closed under
// prerequisites and sorted.
func TeamAdminPermissions() []string {
	var out []string
	for name, socle := range Catalog {
		if socle != PermRoot {
			out = append(out, name)
		}
	}
	return Closure(out)
}

// ValidateCustomPermissions checks a proposed custom-role permission set
// (ADR-038) and returns it closed under prerequisites, ready to persist. Three
// rules, in order:
//
//   - every entry is a real catalogue permission (a typo would make a role that
//     grants nothing, or worse, drifts from the contract);
//   - none is instance-scoped (socle root): a custom role is team-scoped and can
//     NEVER grant instance:* / root, whoever composes it;
//   - the CLOSED set is within the composer's own permissions — anti-elevation:
//     you cannot mint a role more powerful than yourself. The closure is checked,
//     not the raw input, so a prerequisite the composer lacks is caught too.
//
// It returns the closed set, or an error naming the first offending permission.
func ValidateCustomPermissions(perms []string, composer []string) ([]string, error) {
	for _, p := range perms {
		socle, ok := Catalog[p]
		if !ok {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		if socle == PermRoot {
			return nil, fmt.Errorf("permission %q is instance-scoped and cannot be granted to a custom role", p)
		}
	}
	closed := Closure(perms)
	for _, p := range closed {
		if !Has(composer, Permission(p)) {
			return nil, fmt.Errorf("you cannot grant %q, a permission you do not hold", p)
		}
	}
	return closed, nil
}

// ExpandGranular closes a granular permission set under its prerequisites and
// adds the coarse socle of each permission, so the identity satisfies BOTH the
// granular endpoint checks and the few endpoints still declared with a coarse
// permission (getVersion=read, the webhook receivers=deploy). Deduplicated,
// sorted.
func ExpandGranular(perms []string) []string {
	set := map[string]bool{}
	for _, p := range Closure(perms) {
		set[p] = true
		if socle, ok := Catalog[p]; ok {
			set[string(socle)] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// EffectivePermissions expands a caller's coarse scopes (a role's set, or a
// token's scopes) into the granular permissions it holds, AND keeps the coarse
// scope strings themselves. Keeping both is what makes the ADR-038 migration
// incremental: endpoints not yet converted still check a coarse permission
// (require(PermWrite)), while converted ones check a granular one
// (require("applications:update")) — the identity satisfies both. Deduplicated
// and sorted.
func EffectivePermissions(coarse []string) []string {
	scopes := make([]Permission, 0, len(coarse))
	for _, c := range coarse {
		scopes = append(scopes, Permission(c))
	}
	set := map[string]bool{}
	for _, c := range coarse {
		set[c] = true
	}
	for _, g := range ProjectScopes(scopes) {
		set[g] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// splitPerm splits "domaine:action"; ok is false without exactly one colon.
func splitPerm(perm string) (domain, action string, ok bool) {
	for i := 0; i < len(perm); i++ {
		if perm[i] == ':' {
			if i == 0 || i == len(perm)-1 {
				return "", "", false
			}
			// Reject a second colon.
			for j := i + 1; j < len(perm); j++ {
				if perm[j] == ':' {
					return "", "", false
				}
			}
			return perm[:i], perm[i+1:], true
		}
	}
	return "", "", false
}
