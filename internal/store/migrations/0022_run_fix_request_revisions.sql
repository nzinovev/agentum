-- +goose Up
-- IF NOT EXISTS: an unreleased revision of 0021 already added the first column
-- on some development databases.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS active_fix_request_revision_id uuid;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS fix_request_origin_revision_id uuid;

-- +goose Down
ALTER TABLE runs DROP COLUMN fix_request_origin_revision_id;
ALTER TABLE runs DROP COLUMN active_fix_request_revision_id;
