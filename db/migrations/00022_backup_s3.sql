-- Where a backup landed in the bucket. `uploaded_to_s3` alone cannot answer
-- "which object do I restore from", and reconstructing the key from the
-- filename would break the day the plan's prefix changes.

-- +goose Up
ALTER TABLE backup_executions ADD COLUMN s3_key text;

-- +goose Down
ALTER TABLE backup_executions DROP COLUMN s3_key;
