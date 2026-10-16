-- +goose Up
ALTER TABLE runs ADD COLUMN failure_log text NOT NULL DEFAULT '';
ALTER TABLE stage_invocations ADD COLUMN error text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE stage_invocations DROP COLUMN error;
ALTER TABLE runs DROP COLUMN failure_log;
