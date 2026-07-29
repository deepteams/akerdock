-- The NAME of what an audited action touched (§23.4).
--
-- The trail records `target_kind` + `target_uuid`, which answers "which row"
-- but not "which resource": a reader sees `application` and a uuid where they
-- needed `application varuna`. Resolving the uuid at read time would answer
-- with the name the resource has TODAY — and answer nothing at all for a
-- resource that has since been deleted, which is precisely the case an audit
-- reader is investigating.
--
-- So the name is captured when the action happens and never touched again,
-- like every other column of this append-only table. Rows written before this
-- migration keep a NULL: the trail says what it knew, and the uuid remains.

-- +goose Up
ALTER TABLE audit_events ADD COLUMN target_name text;

-- +goose Down
ALTER TABLE audit_events DROP COLUMN target_name;
