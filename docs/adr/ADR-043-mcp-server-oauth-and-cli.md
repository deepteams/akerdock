# ADR-043 — Built-in MCP server: read-only tools, OAuth for remote clients, `akerdock mcp` for local ones

- **Status**: Accepted
- **Date**: 2026-07-28
- **Related PRD sections**: §12, §10.3, §16.1, §21, §27.7
- **Builds on**: [ADR-021](ADR-021-compose-distribution-two-services.md) (single binary, single port), [ADR-031](ADR-031-cli-login-poll-code-pkce.md) (browser login, PKCE), [ADR-038](ADR-038-roles-model.md) (`viewer` role)

## Context

PRD §12 specifies a built-in MCP server: Streamable HTTP on `/mcp`, API-token
authentication, per-team scoping, ten read-only tools, bounded pagination. The
threat model already lists the MCP integration as an actor holding a `read`
token, and ADR-038 designed the `viewer` role for exactly this consumer.

What the PRD does not settle is **how an MCP client authenticates**. Two client
families exist in practice and they are not interchangeable:

- **Remote clients** (a hosted assistant, a browser-based one) reach the
  instance over HTTPS and expect the MCP authorization flow: discovery
  metadata, dynamic client registration, an authorization code with PKCE.
  Pasting a long-lived API token into a third-party service is precisely what
  that flow exists to avoid.
- **Local clients** (a desktop assistant, an IDE) speak stdio to a command.
  For them the natural credential is the one the operator already holds —
  the CLI context in `~/.akerdock/`, or an API token.

## Decision

One MCP surface, two authenticated paths, no write operations.

1. **Read-only, ten tools** (PRD §12): `overview`, list/get for servers,
   projects, applications, databases and services. No tool mutates anything —
   no deploy, no restart, no secret. Values of environment variables and every
   `*_enc` column are never read, let alone returned. Write operations are a
   separate decision, deliberately deferred.
2. **Transport**: Streamable HTTP on `/mcp`, on the control plane's single
   port (§16.1(6)), JSON-RPC 2.0. Implemented in-repo rather than with an SDK:
   the read-only server is a handful of methods (`initialize`, `tools/list`,
   `tools/call`, `ping`), and a young third-party dependency in a distroless
   static binary (ADR-021) costs more than it saves.
3. **Remote clients — OAuth 2.1**: protected-resource metadata
   (`/.well-known/oauth-protected-resource`), authorization-server metadata
   (`/.well-known/oauth-authorization-server`), dynamic client registration,
   authorization code **with mandatory PKCE**, and short-lived opaque access
   tokens (`akdm_`, stored hashed like every other credential). The consent
   screen runs on the panel origin and is authorized by the AkerDock session —
   the same trust anchor as preview SSO (ADR-030) and CLI login (ADR-031). A
   grant is bound to ONE team and carries read permissions only, whatever the
   granting user's role.
4. **Local clients — `akerdock mcp`**: a CLI mode that speaks MCP over stdio
   and forwards to the instance's `/mcp`, using the current CLI context and
   its token (`~/.akerdock/`, ADR-033), or `--token`/`AKERDOCK_TOKEN`. No
   second protocol implementation: the CLI is a transport adapter, the server
   stays the single source of behavior.
5. **Off by default, instance-level**: the MCP surface answers `404` until an
   instance root enables it (PRD §12). Enabling it is audited.

## Alternatives considered

- **API token only** (the PRD's literal wording): rejected as the sole option —
  it forces a long-lived, team-scoped credential into third-party services.
  It remains supported, because it is exactly right for a local client and for
  CI.
- **OAuth only**: rejected — it would make the local/stdio case require a
  browser dance for a tool running on the operator's own machine, where the
  CLI context already exists.
- **Reusing the CLI login flow (poll+code, ADR-031) for MCP clients**: rejected
  — MCP clients implement the standard authorization flow; a bespoke one would
  work with none of them off the shelf.
- **An MCP SDK dependency**: rejected for now (see decision 2); reassessable
  when write tools arrive and the protocol surface grows.
- **Write tools in this version**: rejected. An assistant that can deploy or
  restart is a different risk conversation, and it needs confirmation
  semantics, idempotency and audit design of its own.

## Consequences

- **Positive**: an assistant can inventory and diagnose an instance without a
  human relaying `docker inspect` output; remote clients get a standard,
  revocable, short-lived credential instead of a pasted token; local clients
  get zero configuration beyond an existing CLI context.
- **Negative**: the instance gains a small OAuth authorization server to
  maintain (registration, codes, tokens, expiry) — bounded by being read-only
  and single-purpose; one more surface to keep out of the SPA's catch-all
  route (the `/agent/` lesson of ADR-040).
- **Accepted risks**: dynamic client registration is open by design (any
  client may register) — mitigated by the fact that registration grants
  nothing: only a user's explicit consent, under their session, mints a token
  bound to their team and to `read`; a token leak exposes read-only inventory
  of one team, masked of every secret, and is revocable from the panel.
