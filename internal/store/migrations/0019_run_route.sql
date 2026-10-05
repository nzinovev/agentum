-- +goose Up
ALTER TABLE runs ADD COLUMN route_source text;
ALTER TABLE runs ADD CONSTRAINT runs_route_source_known CHECK (route_source IN ('request', 'triage', 'fallback'));
ALTER TABLE runs ADD COLUMN route_reason text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN route_fallback_code text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN route_fallback_message text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN route_decided_at timestamptz;
ALTER TABLE runs ADD COLUMN route_triage_invocation_id uuid REFERENCES stage_invocations(id);
UPDATE runs SET route_source = 'request', route_reason = 'pipeline_pack was set before route selection', route_decided_at = created_at;

-- +goose Down
ALTER TABLE runs DROP COLUMN route_triage_invocation_id;
ALTER TABLE runs DROP COLUMN route_decided_at;
ALTER TABLE runs DROP COLUMN route_fallback_message;
ALTER TABLE runs DROP COLUMN route_fallback_code;
ALTER TABLE runs DROP COLUMN route_reason;
ALTER TABLE runs DROP COLUMN route_source;
