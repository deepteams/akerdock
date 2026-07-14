-- Crash recovery vs. failure (§2.5 vs §21.1). A deployment gets max_attempts=1:
-- a FAILED deployment is terminal and must never be retried blindly. But a
-- worker that CRASHES has not failed the deployment — it simply never finished
-- it, and INV-013 says the job survives any crash.
--
-- The two were conflated: the reaper counted a crashed attempt as a used-up
-- attempt and dead-lettered the job. resume_count separates them — a crash
-- gives the attempt back, and only a bounded number of times, so a job that
-- kills its worker every time (a poison pill) still ends up in the dead letter
-- instead of looping forever.

-- +goose Up
ALTER TABLE jobs ADD COLUMN resume_count integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN resume_count;
