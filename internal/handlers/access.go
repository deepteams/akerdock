// Access review (ADR-046 §9): who holds platform permissions on a resource, and
// what a given member reaches.
//
// The two readings come from ONE place — the same permission resolution that
// authorizes a request, evaluated with the subject unbound instead of the
// resource. Nothing is stored: a denormalized copy of who-can-see-what drifts
// from the rules it claims to summarize, and a stale access review is worse
// than none, because it asserts a safety nobody checked.
//
// Until scoped assignments exist (ADR-046 §1), every row's scope reads `team` —
// which is the truth of the current model, and the point of shipping this view
// first: it makes the absence of partitioning visible to people who assumed
// otherwise.
package handlers

import (
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
)

// scopeTeam is the canonical rendering of the team scope (ADR-046 §10). Project
// and environment scopes join it once assignments carry them.
const scopeTeam = "team"

// capability is the review-time summary of a set of granular permissions: the
// reading an operator can act on, without them having to hold the catalogue in
// their head. The granular permission stays the unit of evaluation.
type capability struct {
	label string
	perm  auth.Permission
}

// capabilitiesFor names, per resource kind, what is worth reporting. Order is
// the order of the rendered list: what you can see, then what you can do to it,
// then what you can extract from it.
func capabilitiesFor(kind string) []capability {
	switch kind {
	case "application":
		return []capability{
			{"view", auth.PermApplicationsRead},
			{"deploy", auth.PermApplicationsDeploy},
			{"manage", auth.PermApplicationsUpdate},
			{"delete", auth.PermApplicationsDelete},
			{"secrets", "secrets:reveal"},
			{"terminal", auth.PermTerminalOpen},
		}
	case "database":
		return []capability{
			{"view", auth.PermDatabasesRead},
			{"deploy", auth.PermDatabasesLifecycle},
			{"manage", auth.PermDatabasesUpdate},
			{"delete", auth.PermDatabasesDelete},
			{"secrets", "databases:credentials"},
			{"terminal", auth.PermTerminalOpen},
		}
	case "service":
		return []capability{
			{"view", auth.PermServicesRead},
			{"deploy", auth.PermServicesDeploy},
			{"manage", auth.PermServicesManage},
			{"secrets", "secrets:reveal"},
			{"terminal", auth.PermTerminalOpen},
		}
	case "environment":
		return []capability{
			{"view", auth.PermEnvironmentsRead},
			{"deploy", auth.PermApplicationsDeploy},
			{"manage", auth.PermEnvironmentsManage},
			{"secrets", "secrets:reveal"},
		}
	default: // project
		return []capability{
			{"view", auth.PermProjectsRead},
			{"deploy", auth.PermApplicationsDeploy},
			{"manage", auth.PermProjectsManage},
			{"secrets", "secrets:reveal"},
		}
	}
}

// capabilitiesOf keeps the capabilities the permission set actually grants.
func capabilitiesOf(perms []string, kind string) []string {
	var out []string
	for _, c := range capabilitiesFor(kind) {
		if auth.Has(perms, c.perm) {
			out = append(out, c.label)
		}
	}
	return out
}

// accessView assembles the answer for one resource kind. The caller has already
// resolved the resource, so the team boundary and its 404 are behind us.
func (a *API) accessView(w http.ResponseWriter, r *http.Request, id *auth.Identity, kind string) {
	// Seeing WHO ELSE can reach something you already read is the "who do I
	// ask" question; hiding it protects nothing. It still needs members:read.
	if !auth.Has(id.Permissions, auth.PermMembersRead) {
		httpapi.WriteError(w, r, http.StatusForbidden, httpapi.CodeForbidden,
			"members:read is required to see who can reach a resource")
		return
	}

	entries := make([]api.AccessEntry, 0, 16)

	members, err := a.Store.ListTeamMembersForAccess(r.Context(), id.TeamID)
	if err != nil {
		a.internalError(w, r, "access review", err)
		return
	}
	for _, row := range members {
		m := memberFromList(row)
		perms := memberPermissions(m)
		caps := capabilitiesOf(perms, kind)
		if len(caps) == 0 {
			// No capability on this kind of resource — a reviewer on an
			// application, for instance. Listing them as "has access to
			// nothing" would bury the rows that matter.
			continue
		}
		entries = append(entries, api.AccessEntry{
			SubjectKind:  "user",
			SubjectUuid:  ptr(uuidString(m.uuid)),
			SubjectName:  m.email,
			Role:         memberRoleLabel(m),
			Scope:        scopeTeam,
			Capabilities: caps,
		})
	}

	// Instance roots that are not members: they reach every team (rbac-matrix
	// §3.9) and are listed apart, never blended into the team's members.
	if roots, err := a.Store.ListInstanceRootsForAccess(r.Context(), id.TeamID); err == nil {
		for _, root := range roots {
			entries = append(entries, api.AccessEntry{
				SubjectKind:  "instance_root",
				SubjectUuid:  ptr(uuidString(root.Uuid)),
				SubjectName:  root.Email,
				Role:         "instance root",
				Scope:        "instance",
				Capabilities: capabilitiesOf([]string{string(auth.PermRoot)}, kind),
			})
		}
	}

	// A token holds real, durable and rarely watched access, so a review that
	// omits it reassures without grounds — but its owner and expiry are
	// administrative facts, hence the second permission.
	tokensIncluded := auth.Has(id.Permissions, auth.PermTokensRead)
	if tokensIncluded {
		tokens, err := a.Store.ListApiTokensForAccess(r.Context(), id.TeamID)
		if err != nil {
			a.internalError(w, r, "access review", err)
			return
		}
		for _, t := range tokens {
			// A token never reaches more than its creator (ADR-046 §7); with
			// the creator gone, only the token's own scopes remain.
			perms := auth.EffectivePermissions(t.Permissions)
			caps := capabilitiesOf(perms, kind)
			if len(caps) == 0 {
				continue
			}
			entry := api.AccessEntry{
				SubjectKind:       "token",
				SubjectUuid:       ptr(uuidString(t.Uuid)),
				SubjectName:       t.Name,
				Role:              joinScopes(t.Permissions),
				Scope:             scopeTeam,
				Capabilities:      caps,
				TokenCreatorEmail: t.CreatorEmail,
			}
			if t.ExpiresAt.Valid {
				entry.TokenExpiresAt = &t.ExpiresAt.Time
			}
			if t.LastUsedAt.Valid {
				entry.LastUsedAt = &t.LastUsedAt.Time
			}
			entries = append(entries, entry)
		}
	}

	// Most privileged first: the rows an operator must justify are the ones
	// they should read before their attention runs out.
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].Capabilities) > len(entries[j].Capabilities)
	})

	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data           []api.AccessEntry `json:"data"`
		TokensIncluded bool              `json:"tokens_included"`
	}{entries, tokensIncluded})
}

// accessMember is the material the review needs about one member. sqlc emits a
// distinct struct per query; the resolution must depend on the facts, not on
// which query happened to produce them.
type accessMember struct {
	role        store.TeamRole
	uuid        pgtype.UUID
	email       string
	isRoot      bool
	customName  *string
	customPerms []string
}

func memberFromList(m store.ListTeamMembersForAccessRow) accessMember {
	return accessMember{m.Role, m.UserUuid, m.Email, m.IsRoot, m.CustomRoleName, m.CustomPermissions}
}

func memberFromGet(m store.GetTeamMemberForAccessRow) accessMember {
	return accessMember{m.Role, m.UserUuid, m.Email, m.IsRoot, m.CustomRoleName, m.CustomPermissions}
}

// memberPermissions resolves what a member holds, custom role overriding the
// system one — the same rule the session applies at authentication, so the view
// and the enforcement can never disagree.
func memberPermissions(m accessMember) []string {
	if m.isRoot {
		return auth.EffectivePermissions([]string{string(auth.PermRoot)})
	}
	granular := session.PermissionsForRole(m.role)
	if len(m.customPerms) > 0 {
		granular = m.customPerms
	}
	return auth.ExpandGranular(granular)
}

func memberRoleLabel(m accessMember) string {
	if m.isRoot {
		return "instance root"
	}
	if m.customName != nil && *m.customName != "" {
		return *m.customName
	}
	return string(m.role)
}

// joinScopes renders a token's coarse scopes as its "role" column.
func joinScopes(scopes []string) string {
	if len(scopes) == 0 {
		return "token"
	}
	out := scopes[0]
	for _, s := range scopes[1:] {
		out += ", " + s
	}
	return out
}

// GetApplicationAccess implements GET /applications/{uuid}/access.
func (a *API) GetApplicationAccess(w http.ResponseWriter, r *http.Request, applicationUUID api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	if _, ok := a.resolveApplication(w, r, id, applicationUUID); !ok {
		return
	}
	a.accessView(w, r, id, "application")
}

// GetDatabaseAccess implements GET /databases/{uuid}/access.
func (a *API) GetDatabaseAccess(w http.ResponseWriter, r *http.Request, databaseUUID api.DatabaseUuid) {
	id, ok := a.require(w, r, auth.PermDatabasesRead)
	if !ok {
		return
	}
	if _, ok := a.resolveDatabase(w, r, id, databaseUUID); !ok {
		return
	}
	a.accessView(w, r, id, "database")
}

// GetServiceAccess implements GET /services/{uuid}/access.
func (a *API) GetServiceAccess(w http.ResponseWriter, r *http.Request, serviceUUID api.ServiceUuid) {
	id, ok := a.require(w, r, auth.PermServicesRead)
	if !ok {
		return
	}
	if _, ok := a.resolveServiceStack(w, r, id, serviceUUID); !ok {
		return
	}
	a.accessView(w, r, id, "service")
}

// GetProjectAccess implements GET /projects/{uuid}/access.
func (a *API) GetProjectAccess(w http.ResponseWriter, r *http.Request, projectUUID api.ProjectUuid) {
	id, ok := a.require(w, r, auth.PermProjectsRead)
	if !ok {
		return
	}
	if _, ok := a.resolveProject(w, r, id, projectUUID); !ok {
		return
	}
	a.accessView(w, r, id, "project")
}

// GetEnvironmentAccess implements
// GET /projects/{uuid}/environments/{uuid}/access.
func (a *API) GetEnvironmentAccess(w http.ResponseWriter, r *http.Request, projectUUID api.ProjectUuid, environmentUUID api.EnvironmentUuid) {
	id, ok := a.require(w, r, auth.PermEnvironmentsRead)
	if !ok {
		return
	}
	project, ok := a.resolveProject(w, r, id, projectUUID)
	if !ok {
		return
	}
	if _, ok := a.resolveEnvironment(w, r, project, environmentUUID); !ok {
		return
	}
	a.accessView(w, r, id, "environment")
}

// GetMemberAccess implements GET /teams/{uuid}/members/{user_uuid}/access — the
// offboarding question, asked once instead of resource by resource.
func (a *API) GetMemberAccess(w http.ResponseWriter, r *http.Request, teamUUID api.TeamUuid, userUUID api.UserUuid) {
	id, ok := a.require(w, r, auth.PermMembersRead)
	if !ok {
		return
	}
	team, ok := a.resolveTeam(w, r, id, teamUUID)
	if !ok {
		return
	}
	var u pgtype.UUID
	if err := u.Scan(userUUID); err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
		return
	}
	member, err := a.Store.GetTeamMemberForAccess(r.Context(), store.GetTeamMemberForAccessParams{
		TeamID: team.ID, Uuid: u,
	})
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "member not found")
		return
	}

	perms := memberPermissions(memberFromGet(member))
	count, err := a.Store.CountTeamResources(r.Context(), team.ID)
	if err != nil {
		count = 0
	}
	// One row today, because one scope exists. The shape is the scoped one, so
	// adding assignments adds rows rather than rewriting the screen.
	entry := api.MemberAccessEntry{
		Scope:         scopeTeam,
		ScopeLabel:    ptr("team"),
		Role:          memberRoleLabel(memberFromGet(member)),
		Capabilities:  capabilitiesOf(perms, "project"),
		ResourceCount: ptr(int(count)),
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data []api.MemberAccessEntry `json:"data"`
	}{[]api.MemberAccessEntry{entry}})
}
