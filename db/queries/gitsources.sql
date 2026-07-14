-- Git sources (§5.1, data-dictionary §7.1).

-- name: GetDeployKeySource :one
-- One source per (team, deploy key): the API references keys, not sources, so
-- reusing the same key for several applications must not multiply the rows.
SELECT * FROM git_sources
WHERE team_id = $1 AND kind = 'deploy_key' AND private_key_id = sqlc.arg(private_key_id);

-- name: CreateGitSource :one
INSERT INTO git_sources (team_id, name, kind, provider, private_key_id, created_by)
VALUES ($1, $2, $3, $4, sqlc.narg(private_key_id), sqlc.narg(created_by))
RETURNING *;

-- name: GetGitSourceByID :one
SELECT * FROM git_sources WHERE id = $1;

-- name: CountApplicationsUsingPrivateKey :one
-- Applications cloning through a deploy key backed by this key. A key still
-- in use is not deletable (§19.2) — reported as a conflict, not as a foreign
-- key error.
SELECT count(*) FROM applications a
JOIN git_sources gs ON gs.id = a.git_source_id
JOIN resources r ON r.id = a.id
WHERE gs.private_key_id = $1 AND r.deleted_at IS NULL;

-- name: ListPrivateKeyIDsInUse :many
-- Which of these keys are actually referenced — by a server or as an
-- application's deploy key. Answered in one round trip so a key listing does
-- not fan out into one query per row.
SELECT k.id FROM private_keys k
WHERE k.id = ANY(sqlc.arg(key_ids)::bigint[])
  AND (
    EXISTS (SELECT 1 FROM servers s WHERE s.private_key_id = k.id AND s.deleted_at IS NULL)
    OR EXISTS (
      SELECT 1 FROM git_sources gs
      JOIN applications a ON a.git_source_id = gs.id
      JOIN resources r ON r.id = a.id AND r.deleted_at IS NULL
      WHERE gs.private_key_id = k.id)
  );
