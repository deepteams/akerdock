# ADR-059 — The reviewer role can walk the inventory to its previews

- **Status**: Accepted
- **Date**: 2026-08-04
- **Amends**: [ADR-038](ADR-038-roles-model.md) — the `reviewer` permission set only; the
  role model (admin/member/reviewer + custom roles, immutability, closure, anti-elevation)
  stands unchanged
- **Updates**: [rbac-matrix.md](../specs/rbac-matrix.md) §2 (reviewer column and design
  notes), §6.4 (test expectations)
- **Related PRD sections**: §15, §20.4, §23.1

## Context

ADR-038 gave `reviewer` exactly one permission, `previews:read`: someone reviewing a pull
request sees the PR previews and nothing else. The intent was sound — a reviewer has no
business listing the team's databases — but the set is unreachable in practice:

- Previews only exist under `GET /applications/{uuid}/previews`. There is **no team-wide
  previews collection**, and `listApplications` requires `applications:read` — which the
  reviewer does not hold. A reviewer can read the previews of an application whose UUID
  they already know, and has no way inside the product to learn any UUID.
- The dashboard shows it plainly: every sidebar entry is permission-gated, so a reviewer
  signs in to an empty navigation and a default landing page whose list call is refused.

The role works only if a member pastes URLs into the reviewer's chat, which defeats the
point of having the role.

## Decision

`reviewer` holds the **read-only path to its previews**, and nothing else:

```
projects:read → environments:read → applications:read → previews:read
```

- `projects:read`, `environments:read` — walk the drill-down (projects → environments →
  resources) to find the application.
- `applications:read` — list the applications, open one, see its public URL (`domains`).
  By contract this also covers the application's components, storages list, scheduled-task
  list and open-PR list; none of those mutate anything or reveal a secret value.
- `previews:read` — unchanged: the previews, their status, their `fqdn` links, their logs
  and metrics (the spec already scopes preview logs and metrics under this key).

Everything else stays out, deliberately: no `services:read` or `databases:read` (a PR
reviewer has no business in compose stacks or databases), no `deployments:read`, no
`logs:read`/`metrics:read` on applications, no `secrets:read` (INV-003), no writes, no
deploys, no infrastructure. The set is read-only **by construction** — every granted key
is a `:read`, and `ExpandGranular` projects it onto nothing beyond the `read` socle.

A broader read-only profile remains what it was under ADR-038: a **custom role**.

## Consequences

- `session.PermissionsForRole(reviewer)` returns the four keys; tokens minted by a
  reviewer are capped to them (§4.2), and a session narrowed to `reviewer` via ADR-058
  role inspection now shows the real navigation instead of an empty shell.
- The MCP server's tool discovery follows the identity: a reviewer sees the three
  read-only inventory tools its keys cover (`list_projects`, `list_applications`,
  `get_application`) instead of zero.
- The dashboard degrades along the same lines: the Projects drill-down appears; on an
  application the reviewer keeps Overview (with the public URL) and Previews — with the
  preview links clickable — while management tabs and actions stay hidden.
- rbac-matrix §2's reviewer column and its "exactly one permission" design note are
  updated; §6.4's expectation becomes "everything except the four reads → 403/404".
