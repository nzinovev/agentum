-- +goose Up
ALTER TABLE runs ADD COLUMN check_feedback_used boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE runs DROP COLUMN check_feedback_used;
