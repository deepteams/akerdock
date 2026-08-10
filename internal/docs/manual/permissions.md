---
id: permissions
title: What your role lets you do
icon: shield-check
group: Start here
summary: Why a button is missing, and who to ask when it is.
order: 2
---

## The three built-in roles

A role is a named set of granular permissions. The dashboard hides what your role cannot use — including parts of this manual — so a missing page is an answer, not a bug.

| Role | In one line |
| --- | --- |
| admin | Everything in the team: members, roles, tokens, servers, keys — but never the instance settings |
| member | Everything about the resources: apps, databases, stacks, deploys, secrets, backups, previews, tunnels |
| reviewer | Read-only path to the PR previews, and nothing else |

An administrator may also compose a **custom role** with an arbitrary set of permissions; you then see exactly that set.

## What a member deliberately cannot do

These are the walls you will actually meet. None of them is a bug to report — each is a decision:

- **Reveal a secret.** You can write a variable and overwrite it, never read back a masked value (`secrets:reveal` is admin-only). The same holds for a database password (`databases:credentials`) and for a private key.
- **Register or configure a server**, restart its proxy, run a root shell on it. You may open a shell *in a container*, not on the host.
- **Manage members, roles, invitations and team API tokens.** Your own personal token is yours to create, under Personal settings.
- **Declare an external endpoint** for the bastion. You can open a tunnel to one that exists, and request access to it.
- **Change instance settings** — email, sign-in providers, encryption, telemetry. That is the instance root.

> **Note** — An administrator can look at the dashboard through your role — the "View as" entry in the user menu — which is the fastest way to settle "it works for me".

## Working in several teams

A session acts in exactly one team. Switching team from the user menu reloads the dashboard, and the very next request uses the role you hold in the team you switched into. The choice is remembered for your next sign-in.
