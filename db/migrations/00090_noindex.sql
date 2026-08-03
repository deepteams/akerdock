-- Search-engine visibility of a deployed resource (proxy-contract §4.7).
-- Off by default: an application under a custom domain is often a public site
-- whose ranking is the point, and flipping that silently on upgrade would cost
-- traffic no migration can give back. Previews are noindexed unconditionally
-- and carry no column — that one is not an operator choice.

-- +goose Up
ALTER TABLE runtime_configs
    ADD COLUMN noindex boolean NOT NULL DEFAULT false;

ALTER TABLE services
    ADD COLUMN noindex boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE services DROP COLUMN noindex;
ALTER TABLE runtime_configs DROP COLUMN noindex;
