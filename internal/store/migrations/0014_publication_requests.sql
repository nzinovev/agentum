-- +goose Up
ALTER TABLE run_publications ADD COLUMN request_id bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE run_publications DROP COLUMN request_id;
