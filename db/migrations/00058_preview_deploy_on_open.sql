-- Manual-first previews (§20.4.7, ADR-011).
--
-- preview_deploy_on_open: when true (default, historical behaviour) opening a
-- PR auto-deploys its preview. When false, opening a PR only reserves the
-- preview (URL, credential) — the FIRST deployment must be triggered manually
-- from AkerDock or with a `/deploy` comment. Once a preview has been deployed,
-- later pushes keep updating it as usual.

-- +goose Up
ALTER TABLE applications
    ADD COLUMN preview_deploy_on_open boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE applications DROP COLUMN preview_deploy_on_open;
