-- +goose Up
ALTER TABLE runs ADD COLUMN pause_requested_at timestamptz;
ALTER TABLE runs ADD COLUMN previous_result_commit text;
ALTER TABLE runs ADD COLUMN active_fix_request_revision_id uuid;
ALTER TABLE run_publications ADD COLUMN pending_commit text;

-- +goose Down
ALTER TABLE run_publications DROP COLUMN pending_commit;
ALTER TABLE runs DROP COLUMN active_fix_request_revision_id;
ALTER TABLE runs DROP COLUMN previous_result_commit;
ALTER TABLE runs DROP COLUMN pause_requested_at;
