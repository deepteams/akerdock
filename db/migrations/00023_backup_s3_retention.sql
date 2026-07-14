-- Symmetry with local_deleted_at: a backup whose object was purged from the
-- bucket by the S3 retention is no longer restorable from there, and the row
-- must say so rather than keep advertising an object that is gone.

-- +goose Up
ALTER TABLE backup_executions ADD COLUMN s3_deleted_at timestamptz;

-- +goose Down
ALTER TABLE backup_executions DROP COLUMN s3_deleted_at;
