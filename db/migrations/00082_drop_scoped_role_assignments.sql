-- Withdraws the scoped role assignments of ADR-046 (superseded by ADR-047):
-- authorization goes back to one role per member per team, the team staying the
-- isolation boundary.
--
-- Order matters here. Members parked on the `none` base role hold NOTHING once
-- the assignments that were their only access are gone, and they would have no
-- way to understand why: `none` exists solely to be paired with assignments. So
-- they are moved back to `member` BEFORE the table is dropped — a restored
-- access somebody can see and reduce beats a silent lockout.
--
-- The `none` enum value itself stays: PostgreSQL does not remove one, and this
-- schema has never removed an enum value (see 00079, 00065). Nothing writes it
-- after this migration.

-- +goose Up
UPDATE team_memberships SET role = 'member', updated_at = now() WHERE role = 'none';

DROP TABLE IF EXISTS role_assignments;

-- +goose Down
-- Deliberately not reversible: recreating the table would produce an empty one,
-- and the assignments it held are gone. Restoring the feature means restoring
-- ADR-046, which is a decision, not a migration.
SELECT 1;
