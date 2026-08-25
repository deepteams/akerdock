-- A server declares who serves its public routes (ADR-077): `edge_server_id`
-- is a nullable self-reference, not a boolean — the missing property in the
-- box→edge→LAN topology is *inbound reachability*, and it is a relationship
-- between two servers, never a global fact. NULL — the default, and the only
-- thing this migration writes — means "I serve my own routes": the behaviour
-- of every existing server, untouched. The no-chaining and same-team rules
-- live in the API (they need the team scope and the designated edge's own
-- row); the schema only guarantees the reference points at a real server and
-- never cascades a deletion into silently rewiring someone's routing.

-- +goose Up
ALTER TABLE servers
    ADD COLUMN edge_server_id bigint REFERENCES servers (id);
CREATE INDEX servers_edge_server_id_idx ON servers (edge_server_id);

-- +goose Down
ALTER TABLE servers DROP COLUMN edge_server_id;
