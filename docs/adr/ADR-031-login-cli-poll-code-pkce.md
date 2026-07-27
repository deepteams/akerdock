# ADR-031 — Local CLI authentication: poll + confirmation code + PKCE

- **Status**: Accepted
- **Date**: 2026-07-25
- **Related PRD sections**: §12 (official CLI), §10.2/§10.3 (auth, tokens), §23 (security), §5.7 (operations)

## Context

The PRD (§12) plans for an official CLI. AkerDock already knows how to authenticate anyone via
the panel (password, passkey, OIDC/Google), and a **browser session can already create
API tokens** (`POST /teams/{uuid}/tokens`, anti-elevation guard). But there is
**no authentication flow for a non-browser client**: `/auth/oauth/*` is a
strictly browser-based redirect+cookie flow, which produces a session, not a token.

Two structuring constraints of the target environment weigh on the choice:

1. **The client talks only to the manager, on 80/443, and opens no port.** The workstation may
   be behind a NAT, a remote SSH session, a container, a corporate firewall.
   All communication must go out over HTTPS to the instance's FQDN and **traverse an
   intermediate proxy or load balancer**.
2. **Authentication must work for an SSO/OIDC user**, without asking them
   to bypass their usual login method.

## Decision

The CLI authenticates via a **poll + confirmation code bound by PKCE** flow, inspired by
`gh auth login`. It opens **no port** (only outgoing requests), and the final
credential is a **normal `akd_` API token** — named, listed and revocable like the others.

### Sequence

1. The CLI generates `verifier` (32 random bytes, base64url) and
   `challenge = base64url(SHA-256(verifier))`, then `POST /auth/cli/start {challenge, name}`
   (unauthenticated, per-IP rate limiter). Response: `{request_id, user_code, verify_url,
   interval, expires_in}`. `user_code` = 6–8 readable characters; TTL **10 min (proposed
   default)**.
2. The CLI **displays** `user_code` and the URL, then opens the browser at
   `https://<instance>/cli/authorize?request_id=…` (or prints the URL if `--no-browser`).
3. SPA consent page: login if needed (password/passkey/OIDC), team
   selector, requested permissions displayed, **and the `user_code` that the user checks
   against the one in their terminal**. Approval → `POST /auth/cli/approve {request_id, team_uuid,
   permissions}` — **session + double-submit CSRF**; the requested permissions **MUST**
   be a subset of the session's (existing anti-elevation guard).
4. The CLI **polls** `POST /auth/cli/token {request_id, verifier}` (unauthenticated, per-IP
   rate limiter, `interval` backoff): `{status:"pending"}` while not approved; once
   approved, atomic claim (single use) + verification `SHA-256(verifier) == challenge` →
   `akd_` token minted via the **existing token creation path** with the session's
   Identity as creator (the invariant "a caller cannot grant a permission they do
   not hold" applies unchanged). The token is written to
   `~/.akerdock/credentials.yaml` (0600).

### Endpoints (outside the OpenAPI contract, mounted next to `/auth`, like `/terminal/ws`)

- `GET /cli/authorize` — SPA route (consent page), not an API endpoint.
- `POST /auth/cli/start` — unauthenticated, IP rate limiter. Creates the request.
- `POST /auth/cli/approve` — **session + CSRF**. Binds the request to a user/team and
  permissions.
- `POST /auth/cli/token` — unauthenticated, IP rate limiter. Final exchange.

### Normative requirements

- The `request_id` and the codes **MUST** be single-use, short-TTL, and stored
  **hashed** (SHA-256), never in the clear (§23.2).
- The `verifier` **MUST NEVER** pass through the browser: possession of the
  `request_id` alone (visible in the URL, history, proxy logs) **MUST NOT** be sufficient
  to obtain a token — the exchange **MUST** verify `SHA-256(verifier) == challenge`.
- The approval **MUST** be an explicit POST from the consent page, protected by
  the session **and** the double-submit CSRF; never a side effect of a GET.
- The consent page **MUST** display the `user_code` and require the user to
  confirm that it matches the terminal; it **MUST** display the permissions and the
  team, and **MUST** render the `name` inert (a client-controlled string).
- `root`, `deploy` and `read:sensitive` **MUST NOT** be requested by default; the
  default set is `read,write` (`write` is required to mint terminal sessions and
  port-forwards). The page **MAY** allow removing permissions, never adding any.
- The minted token **MUST** carry `expires_at` (TTL **30 days, proposed default**) and a
  recognizable name `cli — <user>@<host>`. No refresh in v1: a `login` (one browser
  round-trip) re-mints.
- `start`, `approve`, `token` (success **and** failure) and the token creation **MUST** be
  audited (§23.4).
- Headless fallback: `akerdock login --with-token` (paste an existing `akd_`) **MUST** exist.

### Transport invariant (stated here, applied by the entire CLI spec)

- The CLI **connects ONLY to** the manager's FQDN, **on 443** (80 only for a
  possible redirect→HTTPS); no other destination.
- The CLI **opens no** inbound or loopback port.
- `shell` and `port-forward` go over `wss://<manager>/…` on 443, with the standard
  WebSocket Upgrade headers (same as the terminal, which already traverses proxies); the tunnel to
  the target server is made **on the manager side** (SSH), invisible to the client.

### Local storage

`~/.akerdock/` (directory `0700`):
- `config.yaml` (`0600`) — contexts `{name → {url, fqdn, team_uuid}}` + `current_context`.
- `credentials.yaml` (`0600`) — `{context → token}`, kept separate so the config is
  inspectable without exposing the tokens.

The OS-native keychain is a **SHOULD (v1.x)**. Acknowledged deviation from the threat model
(which sets it as a "proposed default"): a cross-platform keychain in a static Go binary
(ADR-021 constraint, distroless image) is a real dependency cost; the risk is mitigated
by the `0600` mode, the 30-day TTL, revocability and the visibility of `last_used_at`.

## Alternatives considered

- **Loopback callback (`127.0.0.1:<port>`, gcloud/gh browser style)**: rejected — opens a
  local port (violates the network constraint) and breaks in remote SSH / container / locked-down
  workstation.
- **Bare OAuth Device Authorization Grant** (typed code, no PKCE): rejected — phishable by
  construction (an attacker sends their `user_code` to the victim). Our variant neutralizes
  that vector: it is the CLI that generates the request and displays the code to check.
- **CLI as an OIDC client (driving the OIDC flow itself)**: rejected — does not work for
  password/passkey instances, and would make the CLI a second relying party to configure.
- **Pasted token only**: kept as the `--with-token` fallback, but insufficient as the
  default (poor UX, no integrated SSO).

## Consequences

- **Positive**: a single credential type (`akd_` token), no open port, native
  proxy/LB traversal, SSO for free (approval happens in the browser).
- **Negative**: three more endpoints outside the contract (specified in `docs/specs/cli.md`);
  one DB lookup per poll; two new technical tables (`cli_authorization_codes`).
- **Accepted risks**: token at rest in a `0600` file rather than an OS keychain in
  v1 (see above).
