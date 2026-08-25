-- Models: inference is a first-class resource (ADR-080), mirroring the
-- databases shape — a row per resources(id), an engine enum, an image
-- override, a server the destination must agree with. The typed columns are
-- deliberately few (the knobs deciding whether the model RUNS on this
-- hardware); the rest of the ~200-flag upstream surface lives in
-- engine_flags, an ORDERED jsonb list of {flag, value} pairs, so two
-- deployments diff by flag. memory_fraction is one column because it is one
-- concept — rendered as --gpu-memory-utilization on vLLM and
-- --mem-fraction-static on SGLang. The API key is enveloped like a database
-- credential: it exists to be read back, under models:credentials.
-- published_port is the LAN endpoint (host publish), unique per server the
-- way databases reserve theirs (§22.3).

-- +goose Up
CREATE TYPE inference_engine AS ENUM ('vllm', 'sglang');

ALTER TYPE resource_type ADD VALUE IF NOT EXISTS 'model';

CREATE TABLE models (
    id bigint PRIMARY KEY REFERENCES resources (id) ON DELETE CASCADE,
    engine inference_engine NOT NULL,
    model_id text NOT NULL CHECK (model_id <> ''),
    served_model_name text,
    quantization text,
    max_model_len integer CHECK (max_model_len IS NULL OR max_model_len > 0),
    tensor_parallel_size integer NOT NULL DEFAULT 1 CHECK (tensor_parallel_size > 0),
    memory_fraction real CHECK (memory_fraction IS NULL OR (memory_fraction > 0 AND memory_fraction <= 1)),
    image text,
    image_tag text,
    engine_flags jsonb NOT NULL DEFAULT '[]',
    api_key_enc bytea NOT NULL,
    shm_size_mb integer CHECK (shm_size_mb IS NULL OR shm_size_mb > 0),
    published_port integer NOT NULL CHECK (published_port BETWEEN 1 AND 65535),
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One model per (server, port) — strongly consistent, no double allocation
-- (the databases precedent, §22.3).
CREATE UNIQUE INDEX models_published_port_key ON models (server_id, published_port);
CREATE INDEX models_server_id_idx ON models (server_id);

-- server_id must stay consistent with the destination's server — copied from
-- the databases trigger, same invariant, same reason.
-- +goose StatementBegin
CREATE FUNCTION models_server_consistency() RETURNS trigger AS $$
DECLARE
    destination_server bigint;
BEGIN
    SELECT d.server_id INTO destination_server
    FROM resources r JOIN destinations d ON d.id = r.destination_id
    WHERE r.id = NEW.id;
    IF destination_server IS NULL OR destination_server <> NEW.server_id THEN
        RAISE EXCEPTION 'models.server_id must match the destination server';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER models_server_consistency
    BEFORE INSERT OR UPDATE OF server_id ON models
    FOR EACH ROW EXECUTE FUNCTION models_server_consistency();

-- +goose Down
DROP TABLE models;
DROP FUNCTION models_server_consistency;
DROP TYPE inference_engine;
-- resource_type keeps the 'model' value: PostgreSQL cannot drop an enum value.
