# ADR-058 — A session can inspect a role without a second account

- **Status**: Accepted
- **Date**: 2026-08-03
- **Related**: [ADR-038](ADR-038-roles-model.md), [ADR-046](ADR-046-scoped-role-assignments.md)

## Context

The dashboard degrades per permission: a reviewer sees fewer sections, fewer
buttons and fewer rows than a member. Verifying what a role actually sees
required signing in as somebody who holds it — an invitation, a second mailbox,
a second browser profile, and a real account left behind on the instance.

The result is that the degraded views are the least tested surface of the
product, and regressions in them are found by users rather than by whoever
shipped them.

Two shortcuts were rejected:

- **Client-side simulation** (swap the permission list in the browser): it only
  reproduces what the UI hides. Server-side filtering — narrowed listings, 403s
  — stays invisible, so the preview can claim a role sees something it does not.
- **Impersonating a user account**: it answers a different question ("what does
  Alice see"), and it puts a session under someone else's identity, which is a
  liability in the audit trail and a genuine escalation vector.

## Decision

1. A browser session may carry a **simulated role** — a system role
   (`admin`/`member`/`reviewer`) or one of the team's custom roles — stored on
   the session row (`sessions.view_as_role` / `view_as_custom_role_id`).
2. While set, the session's permissions are the **intersection** of the
   permissions it really holds with the simulated role's. An intersection can
   only remove: no path through this feature grants anything the session did not
   already hold.
3. The instance-root wildcard is dropped by the same intersection, and
   `Identity.InstanceRoot` is false while the mode is on — an inspection that
   keeps `root` shows the root's own view under another name.
4. The **identity is unchanged**: the acting user, the session and every audit
   record still name the real person. This is a role restriction, never an
   impersonation of an account.
5. **Entering** is reserved to the instance root and to team admins, checked
   against the session's **real membership** in its current team — never
   against the permissions the session presents, which the mode may already
   have narrowed. **Leaving is unconditional**: restoring one's own authority
   grants nothing, so it requires no authority to ask for, and an admin demoted
   mid-inspection is not stranded in a role they can no longer choose.
6. The mode's own endpoints (`GET`/`POST /auth/session/view-as`) sit on the
   session surface, outside the permission-checked API: a session narrowed to
   `reviewer` holds no `roles:read` and must still be able to read its state and
   leave.
7. The mode ends with the browser session, and is cleared when the session
   switches team — authority is per team, and a custom role belongs to the team
   it was chosen in.
8. Entering and leaving are audited (`auth.session.view_as`), so an audit reader
   can tell why the actions that follow look unlike their author's usual reach.
9. When the simulated role can no longer be resolved (custom role deleted, team
   mismatch), the session is granted **nothing** rather than silently restored
   to its real powers under a banner still claiming a role.

## Consequences

- Verifying a degraded view costs one menu click, against the real API, with
  real 403s and real filtered listings.
- Nothing here is a security boundary to lean on: an admin inspecting `reviewer`
  keeps their real authority everywhere else — an API token they already hold, a
  second tab signed in elsewhere, the CLI. The mode is an inspection tool, not a
  sandbox, and must never be presented as one.
- One extra query per authenticated request **only while the mode is on**, to
  resolve a simulated custom role.
- The dashboard shows a permanent banner while inspecting; a degraded UI with no
  explanation would be read as a bug.
