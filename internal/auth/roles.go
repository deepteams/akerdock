package auth

import "github.com/deepteams/akerdock/internal/store"

// The system roles' permission sets (ADR-038, rbac-matrix §2). They live in this
// package rather than in internal/session because BOTH doors need them: a
// browser session resolves a member's role, and bearer authentication resolves
// the role of a token's CREATOR to bound what the token may do (§4.2).

// memberPermissions is the granular set of the team `member` role — the old
// "developer" column of the RBAC matrix (rbac-matrix §2): full management of the
// team's resources (apps, databases, services, deploys, backups, previews,
// secrets), but NOT team administration (members/roles/tokens/invitations),
// infrastructure (servers/keys/cloud), instance settings, or root-shell access.
// Closure adds the `:read` prerequisites, so only the acting permissions are
// listed here.
var memberPermissions = []string{
	string(PermTeamRead), string(PermMembersRead),
	string(PermProjectsRead), string(PermProjectsManage),
	string(PermEnvironmentsRead), string(PermEnvironmentsManage),
	string(PermResourcesRead), string(PermResourcesAdopt),
	string(PermApplicationsRead), string(PermApplicationsCreate),
	string(PermApplicationsUpdate), string(PermApplicationsDelete),
	string(PermApplicationsDeploy), string(PermApplicationsLifecycle),
	string(PermApplicationsExec),
	string(PermDatabasesRead), string(PermDatabasesCreate),
	string(PermDatabasesUpdate), string(PermDatabasesDelete),
	string(PermDatabasesLifecycle),
	string(PermServicesRead), string(PermServicesManage), string(PermServicesDeploy),
	string(PermSecretsRead), string(PermSecretsWrite),
	string(PermServersRead), string(PermCertificatesRead), string(PermKeysRead),
	string(PermSourcesRead), string(PermSourcesManage), string(PermRegistriesManage),
	string(PermStoragesManage),
	string(PermBackupsRead), string(PermBackupsManage), string(PermBackupsRestore),
	string(PermDeploymentsRead), string(PermDeploymentsCancel),
	string(PermPreviewsRead), string(PermPreviewsManage),
	string(PermTerminalOpen), string(PermPortForwardsOpen),
	string(PermExternalEndpointsRead),
	string(PermLogsRead), string(PermMetricsRead),
	string(PermNotificationsRead), string(PermNotificationsManage),
	string(PermUptimeRead), string(PermUptimeManage),
	string(PermAuditRead),
}

// PermissionsForRole maps a team role onto its granular permission set (ADR-038,
// rbac-matrix §2). A role is a NAME for a set of permissions; the sets are
// explicit so the matrix and the code cannot silently disagree. The caller runs
// the result through ExpandGranular, which adds the `:read` prerequisites
// and the coarse socle each permission projects onto.
func PermissionsForRole(role store.TeamRole) []string {
	switch role {
	case store.TeamRoleAdmin, store.TeamRoleOwner:
		// admin is the merged owner+admin role (ADR-038): full control of the team
		// and its resources, never instance settings. `owner` is legacy — rows are
		// migrated to `admin`, but map it here too so a stray one is not left
		// powerless.
		return TeamAdminPermissions()
	case store.TeamRoleReviewer:
		// reviewer sees only PR previews — nothing else (ADR-038).
		return []string{string(PermPreviewsRead)}
	case store.TeamRoleNone:
		// Legacy value (ADR-046, withdrawn by ADR-047): 00082 moved every member
		// off it, but a row could survive a partial migration and must not be
		// read as "member" by accident. Nothing writes it any more.
		return nil
	default: // member
		return memberPermissions
	}
}
