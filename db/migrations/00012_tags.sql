-- Tags (data-dictionary §5.4/§5.5): free labels, N-N with resources, used
-- by the deploy webhook (?tag=).

-- +goose Up
CREATE TABLE tags (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    uuid uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    team_id bigint NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name citext NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (team_id, name)
);

CREATE TABLE resource_tags (
    resource_id bigint NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    tag_id bigint NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_id, tag_id)
);

CREATE INDEX resource_tags_tag_id_idx ON resource_tags (tag_id);

-- +goose Down
DROP TABLE resource_tags;
DROP TABLE tags;
