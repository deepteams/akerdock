-- GitHub Apps (data-dictionary §7.2, git-webhook-protocols §2) and the
-- discovered-repositories cache (§7.3).

-- name: CreateDraftGithubApp :one
-- Manifest flow step 1: the draft carries the state token (hashed, one-shot)
-- the callback must present. Credentials arrive at conversion.
INSERT INTO github_apps (team_id, name, api_url, html_url, manifest_state_hash, manifest_state_expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, now() + interval '10 minutes', sqlc.narg(created_by))
RETURNING *;

-- name: GetGithubAppByUUID :one
SELECT * FROM github_apps WHERE uuid = $1 AND team_id = $2;

-- name: GetGithubAppByStateHash :one
-- Callback resolution: the state is single-use and expiring — matched hashed,
-- never logged (INV-003).
SELECT * FROM github_apps
WHERE manifest_state_hash = $1 AND manifest_state_expires_at > now() AND app_id IS NULL;

-- name: CompleteGithubAppConversion :one
-- Conversion (protocols §2.1 step 5): persist what GitHub returned, clear the
-- state so the callback cannot be replayed.
UPDATE github_apps SET
    name = $2, app_id = $3, slug = $4, client_id = $5,
    client_secret_enc = $6, webhook_secret_enc = $7, app_private_key_enc = $8,
    html_url = $9,
    manifest_state_hash = NULL, manifest_state_expires_at = NULL,
    updated_at = now(), version = version + 1
WHERE id = $1 AND app_id IS NULL
RETURNING *;

-- name: SetGithubAppInstallation :execrows
-- First of the two redundant installation signals wins (§2.1 step 7).
UPDATE github_apps SET installation_id = $2, updated_at = now(), version = version + 1
WHERE id = $1;

-- name: ClearGithubAppInstallation :execrows
-- installation deleted/suspended (§2.4): the source is degraded, not removed.
UPDATE github_apps SET installation_id = NULL, updated_at = now(), version = version + 1
WHERE id = $1;

-- name: ListGithubAppsPage :many
SELECT * FROM github_apps
WHERE team_id = $1 AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(page_limit);

-- name: GetGithubAppByAppID :one
-- Webhook routing: X-GitHub-Hook-Installation-Target-ID names the app_id
-- (§2.5) — instance-wide, the signature then proves the sender.
SELECT * FROM github_apps WHERE app_id = $1;

-- name: DeleteGithubApp :execrows
DELETE FROM github_apps WHERE id = $1;

-- name: GetGitSourceForGithubApp :one
SELECT * FROM git_sources WHERE github_app_id = $1;

-- name: CreateGithubAppSource :one
-- One git source per converted app: what applications reference (INV-002).
INSERT INTO git_sources (team_id, name, kind, provider, api_url, html_url, github_app_id, created_by)
VALUES ($1, $2, 'github_app', 'github', $3, $4, $5, sqlc.narg(created_by))
RETURNING *;

-- name: UpsertRepository :one
-- Discovery cache (§7.3): external_id is the identity — a rename updates
-- full_name on the same row.
INSERT INTO repositories (git_source_id, external_id, full_name, default_branch, html_url)
VALUES ($1, $2, $3, sqlc.narg(default_branch), sqlc.narg(html_url))
ON CONFLICT (git_source_id, external_id) DO UPDATE SET
    full_name = excluded.full_name,
    default_branch = excluded.default_branch,
    html_url = excluded.html_url,
    updated_at = now()
RETURNING *;

-- name: DeleteVanishedRepositories :execrows
DELETE FROM repositories
WHERE git_source_id = $1 AND NOT (external_id = ANY (sqlc.arg(external_ids)::text[]));

-- name: ListRepositoriesForSource :many
SELECT * FROM repositories WHERE git_source_id = $1 ORDER BY full_name;

-- name: GetRepositoryByFullName :one
-- Exact match, never a prefix comparison (§23.5): webhook → resource.
SELECT r.* FROM repositories r
JOIN git_sources gs ON gs.id = r.git_source_id
WHERE gs.github_app_id = $1 AND r.full_name = $2;

-- name: GetGithubAppByUUIDAny :one
-- Browser callbacks (manifest redirect, setup) carry no team context: the
-- uuid alone resolves the app; the state/signature proves the caller.
SELECT * FROM github_apps WHERE uuid = $1;

-- name: ListApplicationIDsForRepositoryPush :many
-- Fan-out of an app-level push webhook (protocols §2.4): every application
-- bound to the pushed repository, matched by the provider-side repo ID —
-- exact identity, never a URL comparison (INV-009, §23.5).
SELECT a.id FROM applications a
JOIN repositories repo ON repo.id = a.repository_id
JOIN git_sources gs ON gs.id = repo.git_source_id
JOIN resources r ON r.id = a.id
WHERE gs.github_app_id = $1 AND repo.external_id = $2 AND r.deleted_at IS NULL
ORDER BY a.id;

-- name: GetGithubAppByID :one
SELECT * FROM github_apps WHERE id = $1;

-- name: GetRepositoryByID :one
SELECT * FROM repositories WHERE id = $1;
