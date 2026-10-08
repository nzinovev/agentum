-- +goose Up
ALTER TABLE stage_invocations ADD COLUMN kind text NOT NULL DEFAULT 'stage';
ALTER TABLE stage_invocations ADD CONSTRAINT stage_invocations_kind_known CHECK (kind IN ('stage', 'triage'));
UPDATE stage_invocations SET kind = 'triage' WHERE sequence = 0 AND stage = 'triage';

-- +goose Down
ALTER TABLE stage_invocations DROP CONSTRAINT stage_invocations_kind_known;
ALTER TABLE stage_invocations DROP COLUMN kind;
