-- DNS-01 credentials (proxy-contract §7.2, data-dictionary §6.7). A wildcard
-- certificate cannot be issued over HTTP-01 — the CA has no single host to ask —
-- so a wildcard domain without a DNS credential is a promise the proxy can
-- never keep.
--
-- The credential is a set of environment variables Lego expects
-- (CF_DNS_API_TOKEN, AWS_ACCESS_KEY_ID, …). It is envelope encrypted here, and
-- materialized on the server as /data/akerdock/proxy/acme.env (0600), injected
-- into the proxy container with --env-file: never in a generated config file,
-- never in argv (INV-003/INV-012).

-- +goose Up
CREATE TABLE cloud_credentials (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    name text NOT NULL,
    -- The Lego provider identifier (cloudflare, route53, ovh…). It becomes the
    -- resolver name dns01-<provider>, so it reaches a config file: validated
    -- against a closed grammar at the API edge (INV-012).
    provider text NOT NULL,
    config_enc bytea NOT NULL,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    version integer NOT NULL DEFAULT 1,
    UNIQUE (team_id, name)
);

CREATE INDEX cloud_credentials_team_id_idx ON cloud_credentials (team_id);

ALTER TABLE servers
    ADD COLUMN dns_credential_id bigint REFERENCES cloud_credentials (id) ON DELETE RESTRICT;

ALTER TABLE certificates
    ADD CONSTRAINT certificates_dns_credential_fk
        FOREIGN KEY (dns_credential_id) REFERENCES cloud_credentials (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE certificates DROP CONSTRAINT certificates_dns_credential_fk;
ALTER TABLE servers DROP COLUMN dns_credential_id;
DROP TABLE cloud_credentials;
