-- Certificates: observed reflection of the server state (§18.3).

-- name: UpsertCertificate :exec
INSERT INTO certificates (server_id, kind, main_domain, sans, issuer, not_before, not_after, status, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (server_id, kind, main_domain) DO UPDATE SET
    sans = EXCLUDED.sans, issuer = EXCLUDED.issuer,
    not_before = EXCLUDED.not_before, not_after = EXCLUDED.not_after,
    status = EXCLUDED.status, observed_at = now(), updated_at = now();

-- name: ListCertificatesForServer :many
SELECT * FROM certificates
WHERE server_id = sqlc.arg(server_id)
  AND (sqlc.narg(expiring_within_days)::int IS NULL
       OR (not_after IS NOT NULL AND not_after <= now() + make_interval(days => sqlc.narg(expiring_within_days)::int)))
ORDER BY not_after NULLS LAST, main_domain;

-- name: GetCertificateByUUIDForTeam :one
SELECT sqlc.embed(c), s.uuid AS server_uuid FROM certificates c
JOIN servers s ON s.id = c.server_id
WHERE c.uuid = $1 AND s.team_id = $2 AND s.deleted_at IS NULL;

-- name: SetCertificateStatus :exec
UPDATE certificates SET status = $2, last_error = $3, updated_at = now() WHERE id = $1;

-- name: GetCertificateByID :one
SELECT * FROM certificates WHERE id = $1;

-- name: ListCertificatesToAlert :many
-- Certificates entering a threshold they have not been announced for yet
-- (§4.3). A certificate already alerted at J-30 stays silent until it crosses
-- J-7 — the alert fires on the transition, not on every pass.
SELECT c.*, s.uuid AS server_uuid, t.uuid AS team_uuid
FROM certificates c
JOIN servers s ON s.id = c.server_id
JOIN teams t ON t.id = s.team_id
WHERE c.status IN ('issued', 'expired')
  AND c.not_after IS NOT NULL
  AND c.not_after < now() + make_interval(days => sqlc.arg(threshold_days)::int)
  AND (c.expiry_alerted_threshold IS NULL OR c.expiry_alerted_threshold > sqlc.arg(threshold_days)::int)
ORDER BY c.not_after;

-- name: MarkCertificateAlerted :exec
UPDATE certificates
SET expiry_alerted_threshold = $2, expiry_alerted_at = now()
WHERE id = $1;
