-- Certificates (data-dictionary §6.7, proxy-contract §7): an OBSERVED
-- reflection (§18.3). The real state lives in acme.json and the server's
-- files; this table is synchronized after each proxy apply and by periodic
-- reconciliation. No private key material is ever stored here.

-- +goose Up
CREATE TYPE certificate_kind AS ENUM ('acme_http01', 'acme_dns01', 'custom', 'self_signed');
CREATE TYPE certificate_status AS ENUM ('pending', 'issued', 'renewing', 'failed', 'expired', 'revoked');

CREATE TABLE certificates (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    server_id bigint NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    kind certificate_kind NOT NULL,
    main_domain citext NOT NULL,
    sans citext[] NOT NULL DEFAULT '{}',
    issuer text,
    not_before timestamptz,
    not_after timestamptz,
    status certificate_status NOT NULL DEFAULT 'pending',
    last_error text,
    dns_provider text,
    dns_credential_id bigint,
    cert_path text,
    key_path text,
    observed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (server_id, kind, main_domain),
    CHECK (kind <> 'acme_dns01' OR dns_provider IS NOT NULL)
);

CREATE INDEX certificates_server_id_idx ON certificates (server_id);
-- Expiry is the monitoring signal: J-30/J-7 alerting and the
-- expiring_within_days API filter (§4.3).
CREATE INDEX certificates_not_after_idx ON certificates (not_after);

-- +goose Down
DROP TABLE certificates;
DROP TYPE certificate_status;
DROP TYPE certificate_kind;
