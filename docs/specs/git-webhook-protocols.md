# Specification — Git protocols and webhooks per provider

> Artifact §29.8 of the PRD (`docs/PRD.md`). The PRD is the source of truth; this specification details, provider by provider, the Git integration protocols: webhook reception and validation (INV-009), fork/contributor policy (INV-010), authentication to the APIs, consumed events and rich preview feedback (§20.4.6–20.4.8). Where the PRD is silent, the chosen value is marked **(proposed default)**. The names of headers, events and endpoints are those of the providers' real APIs; uncertain points are marked **(to be verified)**.
>
> Related documents: `docs/specs/deployment-engine.md` (§3.4 coalescing, `superseded` state), `docs/specs/data-dictionary.md` (§7: `git_sources`, `github_apps`, `repositories`, `webhook_endpoints`, `webhook_deliveries`; §8.9: `previews`).

---

## 1. Common reception model

### 1.1 Reception URLs (proposed default)

All incoming webhooks go through the single control plane port (§27.1), outside the `/api/v1` prefix because they are not authenticated by Bearer token but by signature:

| Path | URL | Authentication |
|---|---|---|
| GitHub App (app level, all repos of the installation) | `POST /webhooks/github/apps/{github_app_uuid}` | `X-Hub-Signature-256` (app secret) |
| Manual webhook per application | `POST /webhooks/{provider}/{endpoint_uuid}` with `provider ∈ github\|gitlab\|bitbucket\|gitea` | Provider signature/token (secret from `webhook_endpoints`) |
| Generic deploy webhook (custom CI, §12) | `GET\|POST /api/v1/deploy` | Bearer, `deploy` permission (see §7) |
| GitHub manifest flow callback | `GET /webhooks/github/manifest/callback` | Signed single-use `state` (see §2.1) |

The `endpoint_uuid` (resp. `github_app_uuid`) is a random non-sequential UUID (PRD §19.2): it identifies the target without revealing it and avoids guessable URLs. An unknown UUID answers `404` without a detailed body.

### 1.2 Reception pipeline (§20.3)

Synchronous processing (before the HTTP response) is minimal — target < 500 ms (§16.4):

```text
1. Size limit                 → 413 if exceeded
2. IP allowlist (optional)    → 403 if out of range
3. Endpoint resolution        → 404 if unknown uuid or endpoint disabled
4. Signature verification     → 401 if absent/invalid (constant-time comparison)
5. JSON parse                 → 400 if not parsable
6. Delivery persistence (webhook_deliveries, status = received)
   + deduplication (provider, delivery_id)
7. Immediate 200 response; enqueue of a webhook.process job
──────────────────────────── asynchronous processing ──────────────────────────
8. Exact delivery → application association (INV-009)
9. Policies: auto-deploy, fork/contributor (INV-010), [skip ci]/[skip cd], watch paths
10. Trigger: deployment (coalescing §1.9) / preview / cleanup / command
11. Status update: accepted | ignored (+ ignore_reason) | duplicate | failed
```

Normative details:

- **Size limit**: body rejected beyond **2 MiB** with `413` **(proposed default)** — real push/PR payloads stay far below (GitHub itself caps its payloads at 25 MB and drops the delivery beyond that). The signature is verified on the **complete raw body**; the `payload` column of `webhook_deliveries` is then persisted truncated at **512 KiB** with a truncation marker **(proposed default)**, never any secret inside.
- **IP allowlist**: optional CIDR list per endpoint and/or per instance **(proposed default: disabled)**. It is a complement, never a substitute for the signature (provider ranges change; GitHub publishes its own via `GET /meta`, Atlassian its own for Bitbucket Cloud).
- **Signature**: per-provider algorithm (see the dedicated sections). Comparison is in **constant time** in all cases (standard library, never `==` on strings). An invalid signature is persisted (`signature_valid = false`, `status = failed`) for auditing (§23.4) then answers `401` — it triggers **nothing** (INV-009).
- **Timestamp**: no Git provider signs a timestamp (unlike Stripe/Slack); anti-replay protection therefore rests entirely on the persisted `(provider, delivery_id)` deduplication (UNIQUE constraint of `webhook_deliveries`). Delivery retention (purge §22.2) bounds the dedup window; an overly aggressive purge would reopen the replay window — minimum retention **30 days (proposed default)**.
- **Response**: `200` with minimal body `{"received": true}` as soon as the delivery is persisted, including when asynchronous processing will subsequently ignore it. Rationale: GitLab automatically disables a webhook that fails in bursts, GitHub marks the hook as erroring; business decisions (skip, fork, watch paths) are not delivery errors.

### 1.3 Response codes

| Code | Case | Body |
|---|---|---|
| `200` | Delivery persisted (subsequently processed, ignored or deduplicated) | `{"received": true}` |
| `400` | Unparsable JSON, payload truncated by the network (fails the signature anyway), mandatory event missing | generic |
| `401` | Signature/token absent or invalid | generic, with no hint about the secret |
| `403` | IP outside the allowlist | generic |
| `404` | Unknown or disabled `endpoint_uuid`/`github_app_uuid` | generic |
| `413` | Body > size limit | generic |

No error body distinguishes "wrong secret" from "missing secret", nor "nonexistent endpoint" from "endpoint of another team" (INV-002).

### 1.4 Deduplication

- Key: `(provider, delivery_id)` — `X-GitHub-Delivery`, `X-Gitlab-Event-UUID`, `X-Request-UUID` (Bitbucket), `X-Gitea-Delivery`; UUID generated on the AkerDock side for `generic`.
- A duplicate delivery is persisted with `status = duplicate` **(proposed default: row rejected by the UNIQUE constraint, counted as a metric and logged with a reference to the original, without a second row)** and answers `200`. It never triggers a second deployment (INV-009).
- **Manual redeliveries** ("Redeliver" button on GitHub) keep the same delivery GUID **(to be verified)**: they are therefore absorbed by the dedup. Bitbucket's automatic retries increment `X-Attempt-Number` while keeping the same `X-Request-UUID` **(to be verified)**: absorbed as well.

### 1.5 Exact delivery → resource association (INV-009)

Association is done **by identifier, never by name, never by prefix** (§23.5: "repo with a prefix name" scenario):

| Path | Association keys |
|---|---|
| GitHub App | `github_app_uuid` from the URL → `github_apps`; `installation.id` from the payload = `github_apps.installation_id`; `repository.id` = `repositories.external_id` → applications linked via `applications.repository_id` |
| Manual webhook | `endpoint_uuid` from the URL → `webhook_endpoints.application_id` (exactly one application); consistency check: the payload's repo matches the application's configured repo (`ignored`/`failed` otherwise, never a "closest match" deployment) |
| Generic | UUIDs passed explicitly, resolved within the token's team |

Then filtering by **branch** (`ref == refs/heads/<git_branch>` for a push) or by **PR/MR** (previews). The delivery's `team_id` is that of the associated resource; a delivery that resolves no resource is `ignored` (`ignore_reason = no_target`) **(proposed default)**. It is impossible for a delivery to trigger a resource of another team: the `github_app`/`webhook_endpoint` → application chain carries the ownership (INV-001/INV-002).

If several applications of the same team follow the same repo/branch (a legitimate case, e.g. monorepo), the delivery is associated with **each** of them and each application applies its own policies (watch paths in particular); `webhook_deliveries.application_id` references the first one and the per-application detail is carried by the events/jobs **(proposed default)**.

### 1.6 Fork and contributor policy (INV-010)

Evaluated for every PR/MR event, in this order:

1. **Fork detection**: the PR's source repo differs from the target repo (comparison by **repo ID**, not by name). `previews.is_fork = true`.
2. **Fork PR, `preview_fork_approval_enabled = false`**: ignored (`ignore_reason = fork_untrusted`). No build, no secret, no comment.
3. **Fork PR, approval enabled (§20.4.8)**: ignored until an authorized maintainer has approved (see §2.7). After approval: build on an **isolated builder** (decision §27.5), **no variable marked secret injected** — including those of the preview set; only non-sensitive preview variables are provided **(proposed default)**.
4. **Internal PR, `preview_public_prs_enabled = false`** (default): only authors who are members/collaborators/contributors of the repo trigger (§5.6). Authorship status is verified **server-side via the provider's API** when rights are required (commands, approval); the payload's declarative field (GitHub's `author_association`) suffices only for the parity filter **(proposed default)**.
5. **Internal PR, public PRs enabled**: any PR of the repo triggers.

### 1.7 `[skip ci]` / `[skip cd]` markers (§5.5)

- Searched for in the **head commit message** of the delivery (`head_commit.message` for a push; message of the PR's head commit for a synchronize) **(proposed default)**.
- Case-insensitive comparison, exact markers `[skip ci]` and `[skip cd]` **(proposed default)**; no `[ci skip]` alias in v1 **(proposed default)**.
- Effect: `ignored` (`ignore_reason = skip_ci`). The generic deploy webhook and manual deployment remain usable (the marker only blocks auto-deploy).

### 1.8 Watch paths (§5.5)

- Glob patterns (doublestar `**` syntax **(proposed default)**), one per line (`applications.watch_paths`), evaluated against the union of the `added ∪ modified ∪ removed` files of all the delivery's commits.
- Push payloads **cap the commit list at 20** on GitHub and GitLab: if `total_commits_count > 20`, or in case of a **force push** (the `before → after` chaining does not cover the delivery), the worker queries the provider's compare API (`GET /repos/{owner}/{repo}/compare/{before}...{after}` GitHub, `GET /projects/:id/repository/compare` GitLab) to obtain the complete list **(proposed default)**; when no API is available, the delivery is treated as a "match" (fail-open: better one deployment too many than a missing deployment) **(proposed default)**.
- Watch paths **also apply to previews** (§20.4.5, §15) — the list of a PR's files is obtained via the diff API (`GET /repos/{owner}/{repo}/compare/{base}...{head}` GitHub, `GET /projects/:id/merge_requests/:iid/diffs` GitLab, Bitbucket diffstat, `GET /repos/{owner}/{repo}/pulls/{index}/files` Gitea).
- No match → `ignored` (`ignore_reason = watch_paths`).

### 1.9 Coalescing and `superseded` state

Coalescing is specified in `deployment-engine.md` §3.4; reminder of the contract on the webhook side:

- When a webhook deployment is enqueued for `(application, branch)`, a deployment still `queued` (job not `leased`) originating from a webhook with an older SHA is marked `superseded` (terminal, treated like `cancelled`, `superseded_by` filled in).
- The original delivery of the superseded deployment remains `accepted` and points to the deployment that replaced it — the `webhook_delivery_id` traceability is never lost.
- A `leased`/in-progress deployment is **never** coalesced.
- For previews, if `preview_cancel_obsolete_builds = true` (§20.4.7), an **in-progress** preview build made obsolete by a new commit of the same PR is cancelled (cooperative cancellation) — this is the opt-in extension of coalescing beyond the `queued` state.

### 1.10 Delivery ordering

Providers do not guarantee ordering. Safeguard **(proposed default)**: a push delivery whose `after` is already known as the `before` of a delivery **accepted more recently** for the same `(application, branch)` is ignored (`ignore_reason = out_of_order`). Combined with coalescing and serialization by application lock (deployment-engine §3.1), the residual worst case is a superfluous intermediate deployment, never a silent regression of the deployed SHA after a more recent successful deployment — the deployed SHA is always the `after` of its delivery, resolved at enqueue, never re-resolved.

### 1.11 Observability

Each delivery produces: an audit entry (§23.4: webhook calls), OTLP metrics (counters per provider × status × ignore_reason, reception → `2xx` latency, reception → end-of-processing latency), and the `webhook_delivery_id` is propagated as a correlation down to the deployment, its logs and its notifications (deployment-engine §9).

---

## 2. GitHub — GitHub App (recommended path)

One GitHub App per team (`github_apps` table), created via manifest flow, carrying: the app-level webhook (a single endpoint for all installed repos), API authentication (JWT → installation token) and fine-grained permissions. It is the only path offering the complete rich feedback (§20.4.6): Checks, Deployments, upserted comment, commands.

### 2.1 Creation via manifest flow (complete round trip)

The manifest flow avoids any manual entry (app ID, private key, secret): the instance generates the App, GitHub returns the credentials.

1. **Initiation (dashboard)**: the user picks the team, the target account (personal or organization) and, for GitHub Enterprise Server, the base URL. AkerDock creates the `github_apps` row as a **draft** (generated uuid, NULL credentials) and a signed, single-use `state` token expiring after **10 minutes (proposed default)** — anti-CSRF for the callback.
2. **Manifest submission**: the dashboard renders a self-submitting `POST` form to `https://github.com/settings/apps/new?state={state}` (personal account) or `https://github.com/organizations/{org}/settings/apps/new?state={state}` (organization) — on GHES, same path on the instance's `html_url`. Single form field `manifest` containing the JSON:

```json
{
  "name": "akerdock-<instance>-<suffix>",
  "url": "https://<fqdn-instance>",
  "hook_attributes": { "url": "https://<fqdn-instance>/webhooks/github/apps/<uuid>", "active": true },
  "redirect_url": "https://<fqdn-instance>/webhooks/github/manifest/callback",
  "setup_url": "https://<fqdn-instance>/…/github-apps/<uuid>/setup",
  "public": false,
  "default_events": ["push", "pull_request", "issue_comment"],
  "default_permissions": {
    "contents": "read",
    "metadata": "read",
    "pull_requests": "write",
    "checks": "write",
    "deployments": "write",
    "issues": "read"
  }
}
```

   (`issues: read` only to receive `issue_comment` — see §2.3; the `installation` event is sent to every App by default, without explicit subscription **(to be verified)**.)
3. **Confirmation on GitHub**: the user sees the pre-filled creation page, may adjust the name (global uniqueness of App names), and validates.
4. **Callback**: GitHub redirects to `redirect_url?code={code}&state={state}`. AkerDock verifies the `state` (signature, expiration, single use, match with the draft).
5. **Conversion**: `POST {api_url}/app-manifests/{code}/conversions` — **without authentication**, the `code` is single-use and expires after **one hour** (on GitHub's side). Response: `id` (app ID), `slug`, `client_id`, `client_secret`, `webhook_secret`, `pem` (RSA private key), `html_url`. AkerDock persists: `app_id`, `client_id`, and envelope-encrypts `client_secret_enc`, `webhook_secret_enc`, `app_private_key_enc` (§23.2). The conversion response is never logged (INV-003).
6. **Installation**: AkerDock redirects the user to `https://github.com/apps/{slug}/installations/new`; the user picks the account and the scope ("All repositories" or a selection).
7. **Return**: two redundant signals — the `installation` webhook (`action = created`, `installation.id`, list of repos) received on the app's endpoint, and the browser redirect to `setup_url?installation_id={id}&setup_action=install`. Whichever arrives first fills `github_apps.installation_id`; the UI confirms.
8. **Discovery**: `GET /installation/repositories` (paginated, installation token) feeds the `repositories` cache (`external_id` = GitHub repo ID, stable across renames).

Schema constraint: `github_apps.installation_id` is scalar — **one installation per App record**. Installing the same App on a second account/organization requires a second record (new manifest flow) **(proposed default, aligned with the data dictionary)**.

### 2.2 Authentication: App JWT → installation access token

Two levels, conforming to the GitHub Apps model:

1. **App JWT** — signed **RS256** with the PEM private key. Claims: `iss` = `app_id` (the `client_id` is also accepted by GitHub), `iat` = now − 60 s (clock tolerance), `exp` ≤ `iat` + 10 min (GitHub maximum) — chosen: **`iat` + 9 min (proposed default)**. Used only against the `/app/*` endpoints.
2. **Installation access token** — `POST {api_url}/app/installations/{installation_id}/access_tokens` with `Authorization: Bearer <jwt>`, `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`. Response: `ghs_…` token, `expires_at` = **+1 hour**. The request body MAY restrict the token (`repositories`, `permissions`): AkerDock requests a token **restricted to the target repo(s)** of the operation when it knows them **(proposed default)** — least privilege if the token leaks into a build log.
3. **Cache and renewal**: in-memory cache per `(installation_id, scope)`; early renewal when **< 5 minutes** of validity remain **(proposed default)**; a `401` on an API call invalidates the cache entry and forces a renewal (once only, then classified error). Installation tokens are **never persisted** in the database nor written to a log (INV-003).
4. **Git clone**: `https://x-access-token:{installation_token}@github.com/{owner}/{repo}.git` (or the GHES host). The token is passed to the git process without appearing in the persisted command line (INV-012, deployment-engine).

### 2.3 Minimal requested permissions

| Permission | Level | Rationale |
|---|---|---|
| `contents` | read | Code clone (via `x-access-token`), reading commits and compare API (`GET /repos/{o}/{r}/compare/{base}...{head}`) for watch paths and truncated payloads. Never `write`: AkerDock pushes nothing. |
| `metadata` | read | Mandatory baseline permission of every App; repo discovery (`GET /installation/repositories`), branch/SHA resolution. |
| `pull_requests` | write | Single upserted comment on the PR (`POST/PATCH` issue comments of a PR — covered by this permission for PRs), reading PRs and their files, acknowledgment reactions for commands. |
| `checks` | write | Checks API (`POST/PATCH /repos/{o}/{r}/check-runs`) — reserved for Apps; preview status usable as a merge condition (§20.4.6). |
| `deployments` | write | Deployments API (`POST /repos/{o}/{r}/deployments` + statuses) — "View deployment" button on the PR. |
| `issues` | read | Only to receive the `issue_comment` event (`/deploy`, `/destroy` commands); subscribing to this event requires the Issues or Pull requests permission **(to be verified — if Pull requests suffices, remove `issues` from the manifest)**. |

No organization permission, no `secrets`, `actions`, `administration` access. Any later elevation goes through `new_permissions_accepted` (`installation` event) and an explicit action by the GitHub user.

### 2.4 Consumed webhook events

Subscribed to in the manifest; any other received `X-GitHub-Event` is persisted then `ignored` (`ignore_reason = event_not_handled`) **(proposed default)**.

| `X-GitHub-Event` | Handled actions | Effect |
|---|---|---|
| `push` | — | Auto-deploy of the followed branch (pipeline §1.2); `ref`, `before`, `after`, `commits[]` (capped at 20 — §1.8), `head_commit` |
| `pull_request` | `opened`, `synchronize`, `reopened` | Preview creation/redeploy (§20.4); head SHA = `pull_request.head.sha`; fork if `pull_request.head.repo.id ≠ pull_request.base.repo.id`. If `preview_deploy_on_open = false`, a never-deployed preview is only **reserved** (URL, credential) and awaits a manual deployment (UI or `/deploy`); once engaged (deploying/active/failed), subsequent pushes update it normally |
| `pull_request` | `closed` | Preview cleanup (merge or close — `pull_request.merged` distinguishes); cancellation of in-progress preview builds for this PR |
| `pull_request` | `ready_for_review`, `converted_to_draft` | If `preview_exclude_drafts`: leaving draft → deploy; entering draft → no new deploy **(proposed default: the existing preview is not destroyed)** |
| `pull_request` | `labeled`, `unlabeled` | If `preview_require_label`: label added → deploy, removed → preview destruction **(proposed default)**; also used for fork approval by label (§2.7) |
| `issue_comment` | `created` | `/deploy`, `/destroy` commands (if enabled) and fork approval by comment; only if `issue.pull_request` is present |
| `installation` | `created`, `deleted`, `suspend`, `unsuspend`, `new_permissions_accepted` | Lifecycle: fills/invalidates `installation_id`; `deleted`/`suspend` → source marked degraded + notification (§11) |
| `installation_repositories` | `added`, `removed` | Resynchronization of the `repositories` cache; a removed repo breaks the association of the linked applications → notification. Absent from `default_events`: GitHub systematically delivers the `installation*` events to every App and rejects a manifest that declares them ("Default events unsupported") |

The `edited`, `assigned`, `review_requested`, etc. actions are ignored without processing.

### 2.5 `X-Hub-Signature-256` signature

- Header `X-Hub-Signature-256: sha256=<hex>` — HMAC-SHA256 of the **raw body** with the App's `webhook_secret`; constant-time comparison. The legacy `X-Hub-Signature` header (SHA-1) is ignored.
- Other headers used: `X-GitHub-Delivery` (GUID → `delivery_id`), `X-GitHub-Event`, `X-GitHub-Hook-Installation-Target-ID` (must match `app_id` — additional consistency check **(proposed default)**), `Content-Type: application/json` (the `application/x-www-form-urlencoded` mode is not accepted for the App — the manifest configures JSON).
- Secret absent from the database (draft App) → systematic `401`.

### 2.6 GitHub Enterprise Server

- `github_apps.api_url` (e.g. `https://ghe.example.com/api/v3`) and `html_url` replace the github.com defaults; manifest flow, conversion, JWT, installation tokens and webhooks work identically on GHES (minimum supported versions **(to be verified)**).
- Constructed API URLs are **always** built from `api_url` — never a hardcoded `api.github.com`; SSRF policy (§23.3) applied to `api_url`/`html_url` at registration.

### 2.7 Rich preview feedback (§20.4.6–20.4.8)

Cross-cutting principle: feedback is **best-effort** — a failed feedback call (check, deployment, comment) is logged, retried with backoff, notified if it persists, but **never fails the deployment** of the preview **(proposed default)**.

**a) Checks API** — one check run per preview and per SHA:

- Created as soon as the delivery is accepted: `POST /repos/{owner}/{repo}/check-runs` with `name: "AkerDock / preview / <application_name>"` **(proposed default)**, `head_sha`, `status: "queued"`, `details_url` (deployment page in the dashboard), `external_id` = `deployment_uuid`.
- Transitions: `PATCH /repos/{owner}/{repo}/check-runs/{check_run_id}` — `status: "in_progress"` at build start, then `status: "completed"` with `conclusion: "success"` (with `output.summary` containing the preview URL) or `"failure"`; cancelled/superseded build → `conclusion: "cancelled"`; delivery ignored by policy → `conclusion: "skipped"` **(proposed default)**.
- The check is usable as a required status check of branch protection (merge condition, §20.4.6).

**b) Deployments API** — materializes "View deployment" on the PR:

- `POST /repos/{owner}/{repo}/deployments` with `ref` = head SHA, `environment: "preview/pr-<pr_id>"` **(proposed default)**, `transient_environment: true`, `production_environment: false`, `auto_merge: false`, `required_contexts: []` (otherwise GitHub refuses the deployment if checks are pending — including our own).
- Statuses: `POST /repos/{owner}/{repo}/deployments/{deployment_id}/statuses` with `state` ∈ `in_progress` → `success` (+ `environment_url` = preview URL, `log_url` = AkerDock page) or `failure`; when the preview is destroyed: `state: "inactive"` (removes "View deployment").

**c) Single upserted comment** — never one comment per deployment:

- Invisible HTML marker at the top of the body: `<!-- AkerDock:preview:<application_uuid>:<pr_id> -->` **(proposed default)**.
- Upsert: `GET /repos/{owner}/{repo}/issues/{pr_number}/comments` (paginated), search for the marker; found → `PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}`, otherwise → `POST /repos/{owner}/{repo}/issues/{pr_number}/comments`. The comment ID is memorized in the jobs/events payload (data dictionary §8.9, no dedicated column) to avoid re-reading; the marker search remains the fallback if the memorized ID has disappeared (comment deleted by hand).
- Content: current status, preview URL (with a mention of the access protection §20.4.4), deployed SHA, timestamp, link to the logs. An application with several active previews on the same repo keeps one comment per PR, not per deployment.

**d) Comment commands (opt-in, `preview_comment_commands_enabled`)**:

- `issue_comment` / `created` event, on a PR only (`issue.pull_request` present), body whose **first line** is exactly `/deploy`, `/destroy`, `/rebuild` or `/keep` (trimmed, case-insensitive) **(proposed default)**.
- **Author rights verification, server-side**: `GET /repos/{owner}/{repo}/collaborators/{username}/permission` — requires `permission ∈ {admin, maintain, write}` **(proposed default)**. The payload's `comment.author_association` field is never sufficient (declarative, and `CONTRIBUTOR` covers any author already merged once).
- `/deploy`: (re)deploys the preview at the current head SHA — including for a fork PR **if and only if** the command's author is an authorized maintainer (the command counts as approval, see e). `/destroy`: destroys the preview (`destroying` cycle §8.9). `/rebuild`: like `/deploy` but **without build cache** (`force_rebuild`). `/keep`: rearms the inactivity TTL (§20.4.3) and clears the expiration warning.
- Acknowledgment: `rocket` reaction on the command comment (`POST /repos/{owner}/{repo}/issues/comments/{comment_id}/reactions`, `content: "rocket"`) **(proposed default)**; rights refusal → no reaction, audited event.
- Each command is a normal webhook delivery: deduplicated, audited, traced.

**e) Fork approval (opt-in, `preview_fork_approval_enabled`, §20.4.8)** — three equivalent paths:

1. **Label**: an authorized maintainer (same rights verification as d) applies the configured label — default `AkerDock/approved` **(proposed default)**; the `pull_request`/`labeled` event carries `sender` = the user who applied the label, whose rights are verified via the API.
2. **Comment**: `/deploy` by a maintainer (path d).
3. **Dashboard**: UI approval by an authorized AkerDock user → `previews.fork_approved_by`/`fork_approved_at`. For paths 1–2, `fork_approved_by` remains NULL (the approver is not an AkerDock user) and the GitHub identity is kept in the audit and the event payload **(proposed default)**.

- **The approval is valid for the approved SHA only**: any new push on the fork PR invalidates the approval and puts the preview back into pending-approval state **(proposed default)** — otherwise an attacker pushes safe code, obtains the approval, then pushes the malicious payload (INV-010).
- Even when approved: isolated builder, no secret injected (§1.6).

---

## 3. GitHub — Deploy key + manual webhook

Path without a GitHub App (parity §5.1): code access via SSH key, events via a repo webhook configured by hand.

### 3.1 Deploy key

- AkerDock generates an **ed25519** key pair **(proposed default)** in `private_keys` (encrypted, team-scoped) or imports an existing key; the public key is displayed with a copy button.
- The user adds it to the repo: Settings → Deploy keys → Add deploy key, **without** "Allow write access" (read-only — AkerDock never pushes).
- Clone: `git@github.com:{owner}/{repo}.git` with the key (host key verified according to the instance's SSH policy). A GitHub deploy key is **single-repo**: a key shared across several repos will be refused by GitHub upon submission (key already in use) — generate one key per repo.

### 3.2 Manual repo webhook

- AkerDock creates the `webhook_endpoints` (provider `github`) with a randomly generated secret (256 bits **(proposed default)**), displays the URL `https://<fqdn>/webhooks/github/{endpoint_uuid}` and the secret to copy.
- Configuration on the GitHub side: repo Settings → Webhooks → Add webhook — Payload URL, `Content type: application/json`, Secret, events: `push` + `pull_requests` (previews) + `issue_comment` (commands, if enabled).
- Validation identical to the App: `X-Hub-Signature-256` (HMAC-SHA256, constant time), dedup by `X-GitHub-Delivery`.
- The `ping` event (sent when the hook is created) is persisted and answers `200` with no other effect (`ignore_reason = ping`) **(proposed default)**.

### 3.3 Capability differences

- **No Checks API nor Deployments API**: these APIs require App authentication (checks) or a user token (deployments) that this path does not have.
- **Optional degraded feedback**: if the user provides a token (fine-grained PAT "Commit statuses: write" — or classic `repo:status` scope) **(proposed default: optional token field on the git source)**, AkerDock publishes **commit statuses**: `POST /repos/{owner}/{repo}/statuses/{sha}` with `state` ∈ `pending`/`success`/`failure`/`error`, `context: "AkerDock/preview"` **(proposed default)**, `target_url`. With a broader-scoped PAT (`Pull requests: write`), the upserted comment becomes possible again — same mechanics as §2.7c. **Without a token: no feedback** — the preview works (URL, cleanup) but the PR displays nothing.
- Verifying a command author's rights is impossible without a token → comment commands require a configured token, otherwise they are refused (`ignore_reason = no_api_credentials`) **(proposed default)**.
- Repo discovery unavailable: the user enters the repo URL; `repositories` is not populated, association goes through the application's `webhook_endpoints` (§1.5) with a consistency check on `repository.full_name` (**exact** comparison, case-insensitive **(proposed default)** — never by prefix).

---

## 4. GitLab

### 4.1 Integration

- **API access**: access token (project/group/personal access token, `api` scope) or OAuth **(proposed default: access token in v1, OAuth for dashboard login only)**; stored encrypted on the git source. **Git access**: SSH deploy key (as in §3.1 — GitLab accepts the same key on several projects) or HTTPS clone with the token.
- **Self-hosted GitLab**: `git_sources.api_url` (e.g. `https://gitlab.example.com/api/v4`) and `html_url` configurable; SSRF policy (§23.3).

### 4.2 Webhooks

- Configuration: project → Settings → Webhooks — URL `https://<fqdn>/webhooks/gitlab/{endpoint_uuid}`, "Secret token", checked events: **Push events**, **Merge request events**, **Comments** (Note Hook, if commands enabled).
- **Authentication**: GitLab sends **no HMAC** — the `X-Gitlab-Token` header contains the secret **in cleartext**, compared in constant time to the endpoint's decrypted secret. (TLS is de facto mandatory; this is GitLab's model, not an AkerDock choice.)
- Headers: `X-Gitlab-Event` (`Push Hook`, `Merge Request Hook`, `Note Hook`), `X-Gitlab-Event-UUID` (→ `delivery_id`; **(to be verified)**: kept identical across automatic retries — otherwise the dedup degenerates into a no-op and the §1.10 safeguard takes over), `X-Gitlab-Instance`.
- GitLab **automatically disables** a repeatedly failing webhook (backoff then disabling): one more reason to answer `200` to ignored deliveries (§1.2).

### 4.3 Events

| `X-Gitlab-Event` | Key fields | Effect |
|---|---|---|
| `Push Hook` (`object_kind: push`) | `ref`, `before`, `after`, `checkout_sha`, `commits[]` (capped at 20, `total_commits_count`) | Auto-deploy; watch paths via `commits[].added/modified/removed`, compare API fallback (§1.8) |
| `Merge Request Hook` (`object_kind: merge_request`) | `object_attributes.action` ∈ `open`, `update`, `reopen`, `close`, `merge`; `object_attributes.last_commit.id` (head SHA); `object_attributes.oldrev`; `source_project_id` / `target_project_id`; `work_in_progress`/`draft` | MR previews (§5.6): `open`/`reopen` → deploy; `update` **with `oldrev` present** → redeploy (an `update` without `oldrev` is a title/labels change, ignored); `close`/`merge` → cleanup |
| `Note Hook` (`object_kind: note`) | `object_attributes.noteable_type == "MergeRequest"`, `object_attributes.note`, `merge_request` | `/deploy`, `/destroy` commands and fork approval |

- **Fork**: `object_attributes.source_project_id ≠ object_attributes.target_project_id` (comparison by ID).
- **Command author rights**: `GET /projects/:id/members/all/:user_id` — `access_level ≥ 30` (Developer) **(proposed default)**.

### 4.4 Feedback (parity §20.4.6: commit statuses + upserted note)

- **Commit statuses**: `POST /projects/:id/statuses/:sha` with `state` ∈ `pending`, `running`, `success`, `failed`, `canceled`, `name: "AkerDock/preview"` **(proposed default)**, `target_url`. Displayed in the MR and usable in merge rules (pipelines must succeed applies to external statuses **(to be verified)**).
- **Upserted note** on the MR: identical invisible marker (§2.7c); `GET /projects/:id/merge_requests/:iid/notes` then `PUT /projects/:id/merge_requests/:iid/notes/:note_id`, otherwise `POST /projects/:id/merge_requests/:iid/notes`.
- Deployments API equivalent: the GitLab Environments/Deployments API exists but is **not used in v1** (deliberate degradation, re-evaluable) **(proposed default)**.

---

## 5. Bitbucket (Cloud)

### 5.1 Webhook + secret

- Configuration: Repository settings → Webhooks — URL `https://<fqdn>/webhooks/bitbucket/{endpoint_uuid}`, **Secret** (native support for Bitbucket Cloud "secure webhooks").
- **Signature**: `X-Hub-Signature` header containing `sha256=<hex>` — HMAC-SHA256 of the raw body with the secret **(to be verified: Bitbucket Cloud reuses the historical `X-Hub-Signature` header name with an HMAC-SHA256, without a `-256` variant)**; constant-time comparison. If the webhook was created **without** a secret on the Bitbucket side, no signature is sent: AkerDock refuses (`401`) — the secretless mode is not supported, the Atlassian IP allowlist being only a complement (§1.2).
- Headers: `X-Event-Key`, `X-Hook-UUID` (hook configuration ID), `X-Request-UUID` (→ `delivery_id`), `X-Attempt-Number` (retries).

### 5.2 Events

| `X-Event-Key` | Effect |
|---|---|
| `repo:push` | Auto-deploy: `push.changes[]` with `new`/`old` (branch, `target.hash`); **no per-commit file list** in the payload → watch paths via the diffstat API (see §5.3) |
| `pullrequest:created` | Preview: deploy |
| `pullrequest:updated` | Preview: redeploy **if the head has changed** (`pullrequest.source.commit.hash` different from the last deployed one — Bitbucket also emits `updated` for description edits) **(proposed default)** |
| `pullrequest:fulfilled` | Merge → cleanup |
| `pullrequest:rejected` | Declined → cleanup |
| `pullrequest:comment_created` | `/deploy`, `/destroy` commands and fork approval |

- **Fork**: `pullrequest.source.repository.uuid ≠ pullrequest.destination.repository.uuid`.
- **Short SHA caveat**: Bitbucket payloads carry truncated hashes (12 characters) in some places — the full SHA is resolved via `GET /2.0/repositories/{workspace}/{repo_slug}/commit/{hash}` before enqueue **(proposed default)**.

### 5.3 API access and feedback

- API auth: API token / app password (`repository`, `pullrequest:write` scopes) or OAuth 2.0 **(proposed default: token in v1)**; Git over HTTPS with token or SSH deploy key ("Access keys").
- **Watch paths (degraded)**: `GET /2.0/repositories/{workspace}/{repo_slug}/diffstat/{spec}` (spec `after..before` or PR SHA) to obtain the modified files.
- **Build status** (checks equivalent, degraded): `POST /2.0/repositories/{workspace}/{repo_slug}/commit/{commit}/statuses/build` with `state` ∈ `INPROGRESS`, `SUCCESSFUL`, `FAILED`, `STOPPED`, `key: "akerdock-preview"` **(proposed default)**, `url`. Upsert is native: re-POSTing with the same `key` updates the status.
- **Upserted comment**: `POST /2.0/repositories/{ws}/{slug}/pullrequests/{id}/comments` then `PUT …/comments/{comment_id}`; identical invisible marker (§2.7c) — whether the Bitbucket renderer preserves HTML comments in `content.raw` **(to be verified)**; failing that, a discreet textual marker at the foot of the comment.
- No Deployments API equivalent used (the Bitbucket "deployments" API is tied to Bitbucket Pipelines): not supported.
- **Command author rights**: `GET /2.0/workspaces/{workspace}/permissions/repositories/{repo_slug}?q=user.uuid="{uuid}"` — `permission ∈ {admin, write}` **(proposed default)**.

---

## 6. Gitea / Forgejo

### 6.1 Webhooks

- Configuration: repo (or org) Settings → Webhooks → Gitea/Forgejo — URL `https://<fqdn>/webhooks/gitea/{endpoint_uuid}`, Content type JSON, Secret.
- **Signature**: `X-Gitea-Signature` = hexadecimal HMAC-SHA256 of the raw body (without a `sha256=` prefix); Forgejo sends an equivalent `X-Forgejo-Signature`. Gitea also emits GitHub compatibility headers (`X-GitHub-Event`, `X-Hub-Signature-256`) **(to be verified for `X-Hub-Signature-256` depending on versions)** — AkerDock validates the declared provider's native header first, constant time.
- Headers: `X-Gitea-Event` / `X-Forgejo-Event`, `X-Gitea-Delivery` / `X-Forgejo-Delivery` (→ `delivery_id`).
- The same AkerDock `gitea` endpoint accepts both header families (Forgejo is treated as Gitea, provider `gitea`) **(proposed default)**.

### 6.2 Events and PR previews

| Event | Actions | Effect |
|---|---|---|
| `push` | — | Auto-deploy; GitHub-style payload: `ref`, `before`, `after`, `commits[]` with `added/modified/removed` → native watch paths |
| `pull_request` | `opened`, `synchronized`, `reopened` | Preview: deploy/redeploy (`pull_request.head.sha`) — `synchronized` action (with a "d", differs from GitHub) **(to be verified)** |
| `pull_request` | `closed` | Cleanup (`pull_request.merged` distinguishes merge/close) |
| `issue_comment` | `created` | Commands and fork approval (PR if `issue.pull_request` is present) |

- **Fork**: `pull_request.head.repo.id ≠ pull_request.base.repo.id`.
- **Self-hosted by nature**: `api_url` (e.g. `https://gitea.example.com/api/v1`) and `html_url` on the git source.

### 6.3 API access and feedback

- Auth: access token (`Authorization: token <token>`); Git via SSH deploy key or HTTPS+token.
- **Commit status** (degraded, no Checks API): `POST /repos/{owner}/{repo}/statuses/{sha}` with `state` ∈ `pending`, `success`, `error`, `failure`, `warning`, `context: "AkerDock/preview"` **(proposed default)**, `target_url`.
- **Upserted comment**: issues API — `POST /repos/{owner}/{repo}/issues/{index}/comments`, edit via `PATCH /repos/{owner}/{repo}/issues/comments/{id}`; identical invisible marker (§2.7c).
- **Command author rights**: `GET /repos/{owner}/{repo}/collaborators/{username}/permission` (response `permission` ∈ `admin`/`write`/`read`) **(proposed default)**.

---

## 7. AkerDock generic deploy webhook (§12)

API endpoint for external CIs ("GitHub Actions build → push registry → pull + redeploy" pattern, §5.1):

```
GET|POST /api/v1/deploy?uuid={uuid}[,{uuid}…]&tag={tag}[,{tag}…]&force=true|false
Authorization: Bearer <token>          # `deploy` permission (§10.3)
Idempotency-Key: <key>                 # optional (§24.1)
```

- **Multi-target semantics**: `uuid` accepts a comma-separated list (resource UUIDs); `tag` deploys all resources carrying the tag(s) within the token's team. `uuid` and `tag` are combinable; the target set is the deduplicated union **(proposed default)**. `force=true` = build without cache (deployment-engine §5.2); for an "image" application, force = re-pull.
- **Response**: `200` with one result per target — `{"deployments": [{"resource_uuid": …, "deployment_uuid": …, "message": "queued"} | {"resource_uuid": …, "error": …}]}` **(proposed default)**; an unknown UUID **or one from another team** produces the same generic error entry (INV-002); `404` only if no target resolves. Each accepted deployment follows the engine's `202`-like semantics (job queued, tracked via `deployment_uuid`).
- **Idempotency**: `Idempotency-Key` deduplicates the enqueue (jobs `idempotency_key`, INV-004) — a CI that retries its call does not create two deployments. Without a key, two calls = two deployments (coalescing §1.9 does not apply: it is reserved for webhook deliveries of the same branch) **(proposed default)**.
- **Traceability**: each call is recorded as a `provider = generic` delivery (generated delivery_id, data dictionary §7.5), linked to the created deployments; `deployments.trigger = api` **(proposed default — the canonical vocabulary reserves `webhook` for provider deliveries)**.
- This path **ignores** the auto-deploy policies: `auto_deploy_enabled = false`, `[skip ci]` and watch paths do not apply to it (§1.7) — the caller is authenticated and explicit. The fork policy (INV-010) is moot (no untrusted code: what gets deployed is the resource's config).
- Standard API rate limit (200 req/min, §10.3); the per-server queue cap (`deployment_queue_limit`) answers `429`-like via the engine's cap error (deployment-engine §3.2).

---

## 8. Capability matrix per provider

✔ = supported; ◐ = degraded (lesser mechanism or optional credential required); ✘ = not supported.

| Capability | GitHub App | GitHub deploy key + webhook | GitLab | Bitbucket | Gitea/Forgejo | Generic (§7) |
|---|---|---|---|---|---|---|
| Auto-deploy on push | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ (explicit CI call) |
| PR/MR previews (deploy, redeploy, cleanup) | ✔ | ✔ (without feedback) | ✔ | ✔ | ✔ | ✘ |
| Checks (merge condition) | ✔ Checks API | ◐ commit statuses if PAT provided | ◐ commit statuses | ◐ build statuses | ◐ commit statuses | ✘ |
| Deployments API ("View deployment") | ✔ | ✘ | ✘ (Environments not used in v1) | ✘ | ✘ | ✘ |
| Single upserted comment | ✔ | ◐ if PAT `pull_requests:write` | ✔ MR note | ✔ | ✔ | ✘ |
| `/deploy` `/destroy` commands (opt-in) | ✔ `issue_comment` | ◐ token required to verify rights | ✔ Note Hook | ✔ `pullrequest:comment_created` | ✔ `issue_comment` | ✘ |
| Forks upon approval (§20.4.8) | ✔ label / comment / UI | ◐ UI only (Git rights unverifiable without token) | ✔ note / UI | ◐ comment / UI | ✔ comment / UI | ✘ |
| Watch paths (push) | ✔ (payload + compare API) | ✔ (payload; compare if PAT, otherwise fail-open >20 commits) | ✔ (payload + compare API) | ◐ (diffstat API mandatory) | ✔ (payload) | ✘ (not applicable) |
| Watch paths (previews, §20.4.5) | ✔ compare API | ◐ PAT required, otherwise fail-open | ✔ MR diffs | ◐ diffstat | ✔ files API | ✘ |
| Repo discovery | ✔ | ✘ (manual entry) | ◐ (listing via token, optional v1) | ◐ | ◐ | ✘ |
| Self-hosted / Enterprise | ✔ GHES (`api_url`) | ✔ GHES | ✔ | ✘ (Cloud only; Data Center not covered in v1 **(proposed default)**) | ✔ native | ✔ |

---

## 9. Error handling and provider-side retries

### 9.1 Responses to provider behaviors

- **Replays / redeliveries**: absorbed by the `(provider, delivery_id)` dedup (§1.4) — `200` response, `duplicate` status, zero effect. A replay of a delivery **beyond the retention** (row purged) would pass the dedup: the `out_of_order` safeguard (§1.10) and coalescing bound the effect to a redeployment of the same SHA — idempotent in the product sense.
- **Out-of-order deliveries**: §1.10. For previews, the equivalent is the "payload head SHA == current head SHA of the PR" check; when in doubt (crossed PR events), the worker re-reads the head via the API before enqueueing **(proposed default)**.
- **Truncated/corrupted payloads**: a network truncation invalidates the signature (`401`); invalid JSON with a valid signature (provider bug) → `400`, `failed` delivery, never a partial deployment. Expected fields missing after parse → `failed` with classification, notification if recurring.
- **Webhooks disabled on the provider side** (GitLab auto-disable, GitHub hook in error): undetectable in push mode — a prolonged absence of deliveries for an active source triggers an optional **inactivity alert** **(proposed default: disabled in v1, backlog)**.

### 9.2 Provider API rate limits (budgets and backoff)

- Feedback call budget per preview deployment: ~6 calls (check create/update ×2, deployment + status, comment list/upsert). At 5,000 req/h per GitHub installation (Apps baseline, possibly higher depending on installation size), feedback is not the limiting factor; the **watch paths compare API** on active monorepos can be — short-lived cache of comparisons per `(repo, before, after)` **(proposed default)**.
- All provider clients honor: `429`/`Retry-After`, and on GitHub secondary `403` + `X-RateLimit-Remaining`/`X-RateLimit-Reset`. Bounded retry with exponential backoff + jitter (§22.1), circuit breaker per `(provider, host)` so that a provider outage does not saturate the workers.
- Under rate limit: **feedback** is deferred/abandoned (best-effort, §2.7); the **clone/deployment** is retried according to the engine's policy (deployment-engine §2.4); never an immediate retry loop.
- Safety reserve: suspend non-critical calls (discovery, resync) when `X-RateLimit-Remaining` < 10% **(proposed default)**.

### 9.3 Credential expiration and rotation

- **Expired or revoked PAT/token** (API `401`, HTTPS clone failure): the git source enters a **degraded** state, notification via the channels (§11, `warning` severity), deliveries keep being accepted but actions requiring the API fail cleanly (feedback skipped, watch paths fail-open, commands refused `no_api_credentials`). The error never exposes the token (INV-003).
- **GitHub App private key**: rotation supported — the user generates a new key on GitHub, pastes it into AkerDock (`app_private_key_enc` replaced, old key revoked on GitHub afterwards); subsequent JWTs use the new key, no impact on already issued installation tokens (valid ≤ 1 h).
- **Webhook secret**: two-phase rotation — AkerDock generates the new secret and accepts **both** (old + new) during a **24 h (proposed default)** window while the user updates the provider; the audit records which version validated each delivery.
- **Deploy key removed from the repo**: clone failure classified `auth` → `failed` deployment with explicit remediation, notification.
- **App uninstalled/suspended** (`installation` `deleted`/`suspend`): source degraded immediately + notification; the linked applications refuse auto-deploy with a clear reason.

---

## 10. Test scenarios (basis of the E2E plan, §23.5 / §29.9)

Each scenario targets the complete pipeline (reception → decision → effect), per provider where applicable:

| # | Scenario | Expected result |
|---|---|---|
| T1 | **Replay**: same delivery replayed (same `delivery_id`), including a manual GitHub redelivery | `200`, `duplicate` status, exactly one deployment in total |
| T2 | **Bad signature**: wrong secret, missing signature, signature of another endpoint, body altered by one byte | `401`, `failed` delivery `signature_valid=false`, no trigger, audit present |
| T3 | **Homonymous/prefix repo**: delivery for `org/app-2` while the application follows `org/app` (and the `org/app` vs `org2/app` variant) | No association (comparison by ID/exact), `ignored`/`failed`, never a deployment of the wrong application |
| T4 | **Unapproved fork**: fork PR, approval disabled then enabled without approval | `ignored` `fork_untrusted`; no secret in the builder's environment in any case (assertion on the build env) |
| T5 | **Approved fork then new push**: approval by label, then an additional commit on the fork PR | Preview of the approved SHA only; the new SHA goes back to pending approval |
| T6 | **PR closed during the build**: `closed` received while the preview build is `building` | Build cancelled (cooperative), preview → `destroying` → `destroyed`, deployment `cancelled`, check `cancelled`, GitHub deployment status `inactive` |
| T7 | **Concurrent duplicate delivery**: two simultaneous POSTs of the same `delivery_id` (race on the UNIQUE constraint) | Only one `accepted`, the other `duplicate`, a single deployment |
| T8 | **Out-of-order**: push A→B then push B→C, deliveries received C first then B | B ignored `out_of_order` (or coalesced); final deployed SHA = C |
| T9 | **Coalescing**: 3 rapid pushes while a build occupies the slot | Intermediate deployments `superseded` with `superseded_by`, all deliveries traced, last SHA deployed |
| T10 | **[skip ci]**: push whose head commit contains the marker; then deploy via §7 | `ignored` `skip_ci`; the generic API call deploys anyway |
| T11 | **Watch paths**: monorepo push not touching the patterns; >20-commit push touching the patterns (compare API fallback); force push | `ignored` `watch_paths` / deployed / deployed (documented fail-open) |
| T12 | **Auto Deploy off**: `auto_deploy_enabled=false` | `ignored` `auto_deploy_disabled`; generic deploy webhook functional |
| T13 | **Large/truncated payload**: body > 2 MiB; invalid JSON correctly signed | `413`; `400` + `failed`, no effect |
| T14 | **Unauthorized command**: `/deploy` by an author without write rights (including `author_association=CONTRIBUTOR`) | Silent refusal on the PR side, audit + `ignored`, no deployment |
| T15 | **Single comment**: 3 successive deployments of a preview; manual deletion of the comment in between | A single upserted comment; recreated after deletion (marker fallback) |
| T16 | **Expired credential**: PAT revoked before a preview redeploy | Deployment according to capability (SSH clone ok / classified failure), feedback cleanly skipped, source degraded + notification, no secret in the errors |
| T17 | **Team isolation**: valid delivery targeting an application of another team via a guessed/copied `endpoint_uuid` | Impossible by construction (endpoint → application → team); API test: a team A token cannot create an endpoint on a team B app (INV-002) |
| T18 | **Generic idempotency**: double `POST /api/v1/deploy` with the same `Idempotency-Key`; multi-uuid with one unknown UUID | A single deployment; per-target response with a generic error for the unknown one |

Execution matrix: T1–T3, T7, T13 per provider (GitHub App, manual GitHub, GitLab, Bitbucket, Gitea); T4–T6, T14, T15 on GitHub App + GitLab + Gitea at minimum; all of it in Docker-in-Docker with simulated providers (versioned fixtures of real payloads) in accordance with decision §27.26.
