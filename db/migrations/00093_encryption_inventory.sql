-- Key rotation driven by the SCHEMA rather than by a hand-kept list
-- (ADR-003, §23.2, data dictionary §12).
--
-- The rotation and the `GET /system/encryption` histogram each named their
-- columns one by one: both covered 7 of the 23 encrypted columns. The gap was
-- invisible and actively dangerous — the histogram reported "only the active
-- version remains" while 16 columns still carried the old key, and the runbook
-- reads that signal as permission to delete the old key version, which makes
-- those secrets permanently unreadable (Decrypt fails by design).
--
-- So the inventory is COMPUTED here, once, and everything else derives from it.
-- A future migration that adds a `*_enc` column is rotated and observed the day
-- it is created, with no code to update and nothing to remember.
--
-- The result-returning functions hand back `jsonb` rather than a table: sqlc
-- types scalar function results but not set-returning ones, and the alternative
-- — building SQL identifiers in Go — would put schema-dependent SQL outside the
-- generated layer entirely (ADR-025). The dynamic SQL therefore stays in the
-- database, versioned by this migration.

-- +goose Up

-- +goose StatementBegin
-- encryption_inventory is THE definition: every envelope-encrypted column of
-- the schema and, for each, the expression yielding the row identity bound into
-- the AAD (envelope.aad = table || column || row identity). Three shapes exist,
-- and all three are read off the schema:
--   1. the table has its own `uuid` column — the common case;
--   2. the table shares the primary key of `resources` (applications, services):
--      the identity is the RESOURCE's uuid, which is what the handlers encrypt
--      with;
--   3. neither — the singleton `instance_settings`, whose id is its identity.
-- Output columns are deliberately NOT named table_name/column_name: those names
-- also exist in information_schema.columns and would be ambiguous in the body.
CREATE FUNCTION encryption_inventory()
RETURNS TABLE (tbl text, col text, identity_expr text)
LANGUAGE sql STABLE AS $$
    SELECT c.table_name::text,
           c.column_name::text,
           CASE
               WHEN EXISTS (
                   SELECT 1 FROM information_schema.columns u
                   WHERE u.table_schema = c.table_schema
                     AND u.table_name = c.table_name
                     AND u.column_name = 'uuid'
               ) THEN 't.uuid::text'
               WHEN EXISTS (
                   SELECT 1
                   FROM pg_constraint con
                   JOIN pg_class rel ON rel.oid = con.conrelid
                   JOIN pg_namespace ns ON ns.oid = rel.relnamespace
                   JOIN pg_class frel ON frel.oid = con.confrelid
                   WHERE con.contype = 'f'
                     AND ns.nspname = c.table_schema
                     AND rel.relname = c.table_name
                     AND frel.relname = 'resources'
                     AND con.conkey = ARRAY[(
                         SELECT a.attnum FROM pg_attribute a
                         WHERE a.attrelid = rel.oid AND a.attname = 'id'
                     )]
               ) THEN '(SELECT r.uuid::text FROM resources r WHERE r.id = t.id)'
               ELSE 't.id::text'
           END
    FROM information_schema.columns c
    JOIN information_schema.tables tb
      ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
     AND tb.table_type = 'BASE TABLE'
    WHERE c.table_schema = current_schema()
      AND c.data_type = 'bytea'
      AND c.column_name LIKE '%\_enc'
    ORDER BY 1, 2;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- encryption_inventory_json exposes the inventory itself: [{"tbl","col"}, …].
-- The rotation loop reads it, so the set of columns it rewrites IS the set of
-- columns that exist.
CREATE FUNCTION encryption_inventory_json()
RETURNS jsonb
LANGUAGE sql STABLE AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object('tbl', i.tbl, 'col', i.col)
                              ORDER BY i.tbl, i.col), '[]'::jsonb)
    FROM encryption_inventory() i;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- encryption_key_histogram counts rows per key version across the WHOLE
-- inventory: [{"tbl","col","key_version","row_count"}, …]. The first 4 bytes of
-- every ciphertext carry the key version that wrote it (data dictionary §2.7),
-- so this reads versions without holding any key.
CREATE FUNCTION encryption_key_histogram()
RETURNS jsonb
LANGUAGE plpgsql STABLE AS $$
DECLARE
    inv record;
    part jsonb;
    out_json jsonb := '[]'::jsonb;
BEGIN
    FOR inv IN SELECT * FROM encryption_inventory() LOOP
        EXECUTE format(
            'SELECT coalesce(jsonb_agg(jsonb_build_object(
                        ''tbl'', %1$L::text, ''col'', %2$L::text,
                        ''key_version'', s.key_version, ''row_count'', s.row_count)), ''[]''::jsonb)
               FROM (SELECT ((get_byte(t.%2$I, 0) << 24) | (get_byte(t.%2$I, 1) << 16)
                             | (get_byte(t.%2$I, 2) << 8) | get_byte(t.%2$I, 3))::int AS key_version,
                            count(*)::bigint AS row_count
                       FROM %1$I t
                      WHERE t.%2$I IS NOT NULL
                      GROUP BY 1) s',
            inv.tbl, inv.col)
        INTO part;
        out_json := out_json || part;
    END LOOP;
    RETURN out_json;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- encryption_rotation_candidates returns one batch of rows still encrypted
-- under another key version: [{"row_id","row_aad","ciphertext"}, …], the
-- ciphertext base64-encoded on a single line. Rejecting anything outside the
-- inventory keeps a caller from naming an arbitrary table: %I quotes the
-- identifier, the inventory check decides whether it may be used at all.
CREATE FUNCTION encryption_rotation_candidates(
    p_table text, p_column text, p_active_version int, p_limit int)
RETURNS jsonb
LANGUAGE plpgsql STABLE AS $$
DECLARE
    inv record;
    out_json jsonb;
BEGIN
    SELECT * INTO inv FROM encryption_inventory() i
     WHERE i.tbl = p_table AND i.col = p_column;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'not an envelope-encrypted column: %.%', p_table, p_column;
    END IF;
    EXECUTE format(
        'SELECT coalesce(jsonb_agg(jsonb_build_object(
                    ''row_id'', s.row_id, ''row_aad'', s.row_aad,
                    ''ciphertext'', translate(encode(s.ciphertext, ''base64''), E''\n'', ''''))
                    ORDER BY s.row_id), ''[]''::jsonb)
           FROM (SELECT t.id::bigint AS row_id, (%3$s)::text AS row_aad, t.%2$I AS ciphertext
                   FROM %1$I t
                  WHERE t.%2$I IS NOT NULL
                    AND ((get_byte(t.%2$I, 0) << 24) | (get_byte(t.%2$I, 1) << 16)
                         | (get_byte(t.%2$I, 2) << 8) | get_byte(t.%2$I, 3)) <> $1
                  ORDER BY t.id
                  LIMIT $2) s',
        inv.tbl, inv.col, inv.identity_expr)
    INTO out_json
    USING p_active_version, p_limit;
    RETURN out_json;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- encryption_rotation_apply writes back one re-encrypted value and returns the
-- number of rows written, so a rewrite that hits nothing is visible instead of
-- silently looping forever. It touches the ciphertext and nothing else:
-- re-encryption is not a modification of the data, so `updated_at` is
-- deliberately left alone — a rotation must not look like a user edit.
CREATE FUNCTION encryption_rotation_apply(
    p_table text, p_column text, p_row_id bigint, p_value bytea)
RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE
    inv record;
    written bigint;
BEGIN
    SELECT * INTO inv FROM encryption_inventory() i
     WHERE i.tbl = p_table AND i.col = p_column;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'not an envelope-encrypted column: %.%', p_table, p_column;
    END IF;
    EXECUTE format('UPDATE %1$I SET %2$I = $1 WHERE id = $2', inv.tbl, inv.col)
    USING p_value, p_row_id;
    GET DIAGNOSTICS written = ROW_COUNT;
    RETURN written;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS encryption_rotation_apply(text, text, bigint, bytea);
DROP FUNCTION IF EXISTS encryption_rotation_candidates(text, text, int, int);
DROP FUNCTION IF EXISTS encryption_key_histogram();
DROP FUNCTION IF EXISTS encryption_inventory_json();
DROP FUNCTION IF EXISTS encryption_inventory();
