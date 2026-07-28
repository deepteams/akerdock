package auth

import (
	"fmt"
	"slices"
	"sort"
)

// Scoped authorization (ADR-046 §3, rbac-matrix §3): a role held on the team, on
// a project, or on an environment. The narrowest scope carrying an assignment
// REPLACES the broader one — it does not add to it — which is what makes
// "everything except production" expressible without demoting somebody
// everywhere else.
//
// This file is the resolution itself, deliberately pure: no database, no
// request, no HTTP. The handlers load the material once per request and ask
// here; the access review (ADR-046 §9) asks the same function with the subject
// unbound. Two readings, one implementation — a second one that read "about the
// same" is how a review ends up reassuring about a rule it does not share.

// Scope names where a role is held. The zero value is the team scope, so a
// caller that knows nothing about projects still asks a meaningful question.
type Scope struct {
	ProjectID     *int64
	EnvironmentID *int64
}

// TeamScope is the whole team — the base role's scope.
var TeamScope = Scope{}

// IsTeam reports whether this scope is the team itself.
func (s Scope) IsTeam() bool { return s.ProjectID == nil && s.EnvironmentID == nil }

// String renders the canonical notation used by the API and the audit trail
// (ADR-046 §10). UUIDs are not available here, so this is the internal form;
// the HTTP layer renders `project:<uuid>` from the same data.
func (s Scope) String() string {
	switch {
	case s.EnvironmentID != nil:
		return fmt.Sprintf("environment:%d", *s.EnvironmentID)
	case s.ProjectID != nil:
		return fmt.Sprintf("project:%d", *s.ProjectID)
	default:
		return "team"
	}
}

// Assignment is one role held at one scope: the material the resolution needs,
// with the storage layer's shape left behind.
type Assignment struct {
	Scope Scope
	// Permissions is the role's granular set, already closed under
	// prerequisites (a system role's set or a custom role's stored one).
	Permissions []string
	// RoleName is what the UI and the audit trail display.
	RoleName string
}

// PermissionClass says how a permission behaves when its role is assigned at a
// project or environment scope (rbac-matrix §3.3).
type PermissionClass int

const (
	// ClassScoped is granted on the resources of the scope only.
	ClassScoped PermissionClass = iota
	// ClassTeamRead is granted team-wide even from a scoped assignment: these
	// are working prerequisites — you cannot deploy an application without
	// seeing the server it lands on — and they expose no secret, the sensitive
	// half living in *:reveal / *:credentials (INV-003).
	ClassTeamRead
	// ClassTeamOnly is never conferred by a scoped assignment: team
	// administration, infrastructure mutation, the audit trail, secret
	// revelation, the root shell.
	ClassTeamOnly
)

// teamRead is the class-2 set. Small, closed, and deliberately explicit: a glob
// would silently absorb a future `sources:secret` and hand it to every scoped
// member.
var teamRead = map[string]bool{
	"team:read":          true,
	"members:read":       true,
	"servers:read":       true,
	"certificates:read":  true,
	"keys:read":          true,
	"sources:read":       true,
	"notifications:read": true,
}

// teamOnly is the class-3 set (rbac-matrix §3.3).
var teamOnly = map[string]bool{
	"team:manage": true, "members:manage": true, "invitations:manage": true,
	"roles:read": true, "roles:manage": true,
	"tokens:read": true, "tokens:create": true, "tokens:revoke": true,
	"servers:manage": true, "servers:maintain": true, "servers:proxy": true,
	"certificates:renew": true, "keys:manage": true, "keys:reveal": true,
	"sources:manage": true, "registries:manage": true,
	"cloud:read": true, "cloud:manage": true, "storages:manage": true,
	"templates:manage": true, "config:export": true, "config:apply": true,
	"logs:manage": true, "jobs:manage": true, "audit:read": true,
	"notifications:manage": true, "external-endpoints:manage": true,
	"secrets:reveal": true, "databases:credentials": true, "terminal:root": true,
	// Creating a project has no parent scope to be evaluated against, so the
	// capability is team-only by construction (ADR-046 §4).
	"projects:create": true,
	"instance:manage": true, "instance:audit": true, "instance:encryption": true,
}

// ClassOf returns a permission's class. An unknown name is treated as scoped:
// a permission nobody classified must not silently become team-wide.
func ClassOf(perm string) PermissionClass {
	switch {
	case teamOnly[perm]:
		return ClassTeamOnly
	case teamRead[perm]:
		return ClassTeamRead
	default:
		return ClassScoped
	}
}

// covers reports whether an assignment's scope covers a resource's scope.
//
// A team assignment covers everything; a project assignment covers that project
// and its environments; an environment assignment covers that environment only.
// Resolving an environment needs to know its project, which the caller supplies
// in the resource scope (both ids set) — an environment id alone would make
// "project X covers environment Y" unanswerable.
func (a Assignment) covers(res Scope) bool {
	switch {
	case a.Scope.IsTeam():
		return true
	case a.Scope.EnvironmentID != nil:
		return res.EnvironmentID != nil && *res.EnvironmentID == *a.Scope.EnvironmentID
	case a.Scope.ProjectID != nil:
		return res.ProjectID != nil && *res.ProjectID == *a.Scope.ProjectID
	}
	return false
}

// descendsFrom reports whether this assignment's scope lives under res — an
// environment assignment under its project, anything under the team.
func (a Assignment) descendsFrom(res Scope) bool {
	if res.IsTeam() {
		return true
	}
	if res.EnvironmentID != nil {
		return false // nothing is narrower than an environment today
	}
	return a.Scope.ProjectID != nil && res.ProjectID != nil && *a.Scope.ProjectID == *res.ProjectID
}

// specificity orders scopes: environment (2) beats project (1) beats team (0).
func (s Scope) specificity() int {
	switch {
	case s.EnvironmentID != nil:
		return 2
	case s.ProjectID != nil:
		return 1
	default:
		return 0
	}
}

// Resolve computes the permissions a subject holds on a resource.
//
//   - The most specific scope carrying an assignment wins, and it REPLACES the
//     broader one (ADR-046 §3). Union applies between assignments at that same
//     scope, never across scopes — union across scopes would make the override
//     meaningless, since the broader role would always win by addition.
//   - Team-only permissions are dropped from a scoped assignment; team-read
//     ones are kept from every assignment, wherever it is held.
//
// base is the team-level assignment (always present: every member has a base
// role). scoped are the exceptions. res is the scope of the resource being
// touched — the zero value asks the team-level question, which is what a
// team-level resource (a server, a key) needs.
func Resolve(base Assignment, scoped []Assignment, res Scope) []string {
	// The winning scope: the most specific one that covers the resource.
	best := -1
	var tied []Assignment
	for _, a := range scoped {
		if !a.covers(res) {
			continue
		}
		spec := a.Scope.specificity()
		switch {
		case spec > best:
			best, tied = spec, []Assignment{a}
		case spec == best:
			tied = append(tied, a)
		}
	}

	set := map[string]bool{}
	if best < 0 {
		// No scoped assignment covers this resource: the base role applies in
		// full, which is why an instance with no assignment behaves exactly as
		// it did before scoping existed.
		for _, p := range base.Permissions {
			set[p] = true
		}
	} else {
		// Union at the winning scope only.
		for _, a := range tied {
			for _, p := range a.Permissions {
				if ClassOf(p) != ClassTeamOnly {
					set[p] = true
				}
			}
		}
	}

	// Path visibility: an assignment held BELOW this scope confers the
	// structural reads needed to reach it. Somebody scoped to `staging` has to
	// see the project staging lives in, or their own environment is
	// unreachable — the tree is the only way down to it. Deliberately limited
	// to the two structural reads: it opens a path, never a resource.
	for _, a := range scoped {
		if a.Scope.specificity() > res.specificity() && a.descendsFrom(res) {
			set["projects:read"] = true
			set["environments:read"] = true
		}
	}

	// Team-read permissions are conferred team-wide by ANY assignment the
	// subject holds, including the base role: without them a scoped member
	// cannot see the server their application lands on, and cannot deploy.
	for _, a := range append([]Assignment{base}, scoped...) {
		for _, p := range a.Permissions {
			if ClassOf(p) == ClassTeamRead {
				set[p] = true
			}
		}
	}

	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// CanOn reports whether the subject holds perm on the resource's scope.
func CanOn(base Assignment, scoped []Assignment, res Scope, perm Permission) bool {
	return Has(Resolve(base, scoped, res), perm)
}

// ScopesCovering returns the scopes a subject holds that cover a resource,
// narrowest first — what the access review renders in its "through" column.
func ScopesCovering(base Assignment, scoped []Assignment, res Scope) []Assignment {
	out := []Assignment{}
	for _, a := range scoped {
		if a.covers(res) {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, func(x, y Assignment) int {
		return y.Scope.specificity() - x.Scope.specificity()
	})
	return append(out, base)
}
