---
id: team
title: Team
icon: users
group: Your account and team
summary: Members, invitations, custom roles and shared variables.
order: 1
permission: team:read
gates:
  shared-variables: secrets:read
  members-and-invitations: members:manage
  custom-roles: roles:manage
  audit-log: audit:read
links:
  - label: Members
    route: /team
  - label: Team settings
    route: /settings
---

## Shared variables

Team settings → **Shared variables** holds the values referenced as `{{team.KEY}}`, `{{project.KEY}}` or `{{environment.KEY}}` from a resource’s variables. Declare a value once at the right scope instead of pasting it into six applications.

## Members and invitations

Invite by email; an invitee with no account creates one from the link itself, at the address the invitation names. A pending invitation can be revoked or its link regenerated.

## Custom roles

When admin/member/reviewer do not fit, compose a **custom role** from the permission catalogue and assign it to a member. The three system roles are immutable — deviating means creating a role, not bending an existing one.

## Audit log

Who did what, on which target, with which result — including tunnel and terminal sessions. It is the first place to look when a resource changed and nobody remembers touching it.
