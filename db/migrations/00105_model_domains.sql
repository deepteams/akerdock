-- Optional public domains on models (ADR-080 §1 already anchors models in
-- the resource chain "domains included"; ADR-077 carries them across an edge
-- relay). A model with no domain keeps answering only on the server's LAN
-- address — the URL is optional, never generated.
--
-- The exactly-one-owner CHECK widens from three owner kinds to four, the
-- 00092 move replayed. Routes always target the engine's fixed container
-- port, so target_port stays NULL for model-owned rows.

-- +goose Up
ALTER TABLE domains ADD COLUMN model_id bigint REFERENCES models (id) ON DELETE CASCADE;
ALTER TABLE domains DROP CONSTRAINT domains_one_owner;
ALTER TABLE domains ADD CONSTRAINT domains_one_owner CHECK (
    (application_id IS NOT NULL)::int
    + (service_component_id IS NOT NULL)::int
    + (ingress_endpoint_id IS NOT NULL)::int
    + (model_id IS NOT NULL)::int = 1
);
CREATE INDEX domains_model_id_idx ON domains (model_id);

-- +goose Down
DELETE FROM domains WHERE model_id IS NOT NULL;
ALTER TABLE domains DROP CONSTRAINT domains_one_owner;
ALTER TABLE domains DROP COLUMN model_id;
ALTER TABLE domains ADD CONSTRAINT domains_one_owner CHECK (
    (application_id IS NOT NULL)::int
    + (service_component_id IS NOT NULL)::int
    + (ingress_endpoint_id IS NOT NULL)::int = 1
);
