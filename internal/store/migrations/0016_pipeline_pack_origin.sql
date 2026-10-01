-- +goose Up
-- +goose StatementBegin

-- The origin of the pack a run executed: builtin, project (a replacement or
-- new-name manifest), or project+builtin (inheritance). Pinned once at the
-- run's first effective resolution, from the same pinned base_commit the pack
-- was read from, so later commits cannot rewrite what a running record says
-- it executed. Nullable: a run created but never started has no origin yet.
ALTER TABLE runs ADD COLUMN pipeline_pack_origin text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runs DROP COLUMN pipeline_pack_origin;

-- +goose StatementEnd
