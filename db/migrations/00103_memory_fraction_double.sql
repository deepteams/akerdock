-- memory_fraction was born `real` (00102) and a float32 poisons the rendered
-- command: 0.7 becomes 0.699999988079071 on the serve line, which breaks the
-- export→import round-trip identity ADR-080 §3bis pins by test. The whole
-- chain is float64 now (spec `format: double`, sqlc *float64) — the column
-- follows. Its own migration rather than an edit of 00102: that one is
-- applied on live instances already, and an applied migration is immutable.

-- +goose Up
ALTER TABLE models ALTER COLUMN memory_fraction TYPE double precision;

-- +goose Down
ALTER TABLE models ALTER COLUMN memory_fraction TYPE real;
