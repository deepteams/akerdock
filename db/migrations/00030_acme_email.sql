-- ACME contact email (§4.3). It was derived from the instance FQDN, and fell
-- back to admin@example.com — an address Let's Encrypt refuses. The result is
-- the worst kind of failure: the proxy starts, everything looks fine, and the
-- certificates simply never arrive.
--
-- It is now an explicit setting: seeded from AKERDOCK_ACME_EMAIL at first boot,
-- persisted here, and required before a proxy is bootstrapped.

-- +goose Up
ALTER TABLE instance_settings ADD COLUMN acme_email text;

-- +goose Down
ALTER TABLE instance_settings DROP COLUMN acme_email;
