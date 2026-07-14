-- Managed databases (data-dictionary §9.3/§9.4, PRD §6). v1 = PostgreSQL.
-- server_id is denormalized from the destination (trigger) so the public
-- port reservation can be enforced by a unique constraint — the strongly
-- consistent guarantee of §22.3.

-- +goose Up
CREATE TYPE db_engine AS ENUM ('postgresql', 'mysql', 'mariadb', 'mongodb', 'redis', 'keydb', 'dragonfly', 'clickhouse');
CREATE TYPE public_access_mode AS ENUM ('port_mapping', 'tcp_proxy');

CREATE TABLE databases (
    id bigint PRIMARY KEY REFERENCES resources (id) ON DELETE CASCADE,
    engine db_engine NOT NULL,
    image text,
    image_tag text,
    custom_config text,
    initdb_args text,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    is_public boolean NOT NULL DEFAULT false,
    public_access_mode public_access_mode CHECK (NOT is_public OR public_access_mode IS NOT NULL),
    public_port integer CHECK (public_port BETWEEN 1 AND 65535),
    tcp_proxy_timeout_seconds integer NOT NULL DEFAULT 3600 CHECK (tcp_proxy_timeout_seconds > 0),
    ssl_enabled boolean NOT NULL DEFAULT false,
    ssl_mode text CHECK (ssl_mode IN ('disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full', 'on', 'off')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (NOT is_public OR public_port IS NOT NULL)
);

-- Public port reservation: one database per (server, port) — strongly
-- consistent, no double allocation possible (§22.3).
CREATE UNIQUE INDEX databases_public_port_key ON databases (server_id, public_port) WHERE is_public;

-- server_id must stay consistent with the destination's server.
-- +goose StatementBegin
CREATE FUNCTION databases_server_consistency() RETURNS trigger AS $$
DECLARE
    dest_server bigint;
BEGIN
    SELECT d.server_id INTO dest_server
    FROM resources r JOIN destinations d ON d.id = r.destination_id
    WHERE r.id = NEW.id;
    IF dest_server IS NULL OR dest_server <> NEW.server_id THEN
        RAISE EXCEPTION 'databases.server_id % inconsistent with the destination of resource %', NEW.server_id, NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER databases_server_consistency_trg
    BEFORE INSERT OR UPDATE OF server_id ON databases
    FOR EACH ROW EXECUTE FUNCTION databases_server_consistency();

CREATE TABLE database_credentials (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    database_id bigint NOT NULL REFERENCES databases (id) ON DELETE CASCADE,
    username text NOT NULL,
    password_enc bytea NOT NULL,
    -- Initial database name (PostgreSQL: POSTGRES_DB).
    db_name text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (database_id, username)
);

-- +goose Down
DROP TABLE database_credentials;
DROP TRIGGER databases_server_consistency_trg ON databases;
DROP FUNCTION databases_server_consistency();
DROP TABLE databases;
DROP TYPE public_access_mode;
DROP TYPE db_engine;
