-- Envelope-encryption inventory (ADR-003, data-dictionary §12). The first
-- 4 bytes of every *_enc column carry the key version that encrypted it.
--
-- Nothing here names a table. The inventory is derived from the schema by
-- `encryption_inventory()` (migration 00093) and everything below reads it, so
-- a new *_enc column is rotated and observed without touching this file. A
-- hand-kept list is what let the histogram report a converged rotation while
-- 16 columns still held the old key.

-- name: ListEncryptedColumns :one
-- The inventory itself: [{"tbl","col"}, ...] over every encrypted column of the
-- schema. Drives the rotation loop, so what gets rewritten IS what exists.
SELECT encryption_inventory_json();

-- name: EncryptionKeyVersionHistogram :one
-- [{"tbl","col","key_version","row_count"}, ...] over the WHOLE inventory. A
-- rotation has converged once the active version is the only one left -- a claim
-- that only holds because the inventory is exhaustive by construction.
SELECT encryption_key_histogram();

-- name: EncryptionRotationCandidates :one
-- One batch of rows still on another key version, with the row identity bound
-- into their AAD so the caller can decrypt and re-encrypt them.
SELECT encryption_rotation_candidates(
    sqlc.arg(table_name)::text, sqlc.arg(column_name)::text,
    sqlc.arg(active_version)::int, sqlc.arg(row_limit)::int);

-- name: EncryptionRotationApply :one
-- Writes back one re-encrypted value, ciphertext only; returns rows written.
SELECT encryption_rotation_apply(
    sqlc.arg(table_name)::text, sqlc.arg(column_name)::text,
    sqlc.arg(row_id)::bigint, sqlc.arg(value)::bytea);
