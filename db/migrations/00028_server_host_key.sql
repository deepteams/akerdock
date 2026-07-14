-- SSH host key pinning (§20.1). The fingerprint was observed at validation and
-- thrown away: every later connection trusted whatever answered on the port.
-- That is a man-in-the-middle away from handing an attacker our deploy key and
-- every secret we push to the server.
--
-- Trust-on-first-use: the key seen at validation is pinned, and any later
-- change is refused until an operator re-validates the server deliberately.

-- +goose Up
ALTER TABLE servers ADD COLUMN host_key_fingerprint text;

-- +goose Down
ALTER TABLE servers DROP COLUMN host_key_fingerprint;
