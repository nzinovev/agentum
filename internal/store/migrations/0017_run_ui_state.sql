-- +goose Up
-- +goose StatementBegin

ALTER TABLE runs
    ADD COLUMN stop_reason text NOT NULL DEFAULT '',
    ADD COLUMN error text NOT NULL DEFAULT '',
    ADD COLUMN cancel_reason text NOT NULL DEFAULT '';

UPDATE runs AS run
SET current_stage = COALESCE(run.current_stage, (
        SELECT invocation.stage FROM stage_invocations AS invocation
        WHERE invocation.tenant_id = run.tenant_id AND invocation.run_id = run.id
        ORDER BY invocation.sequence DESC, invocation.id DESC LIMIT 1
    )),
    stop_reason = CASE WHEN run.state IN ('paused_open_questions', 'paused_gate', 'paused_user_stop')
        THEN COALESCE((
            SELECT event.payload->>'stop_reason' FROM events AS event
            WHERE event.tenant_id = run.tenant_id AND event.run_id = run.id
              AND event.type = 'run.state_changed' AND event.payload->>'to' = run.state
            ORDER BY event.id DESC LIMIT 1
        ), (
            SELECT invocation.stop_reason FROM stage_invocations AS invocation
            WHERE invocation.tenant_id = run.tenant_id AND invocation.run_id = run.id
            ORDER BY invocation.sequence DESC, invocation.id DESC LIMIT 1
        ), '') ELSE '' END,
    error = CASE WHEN run.state = 'failed' THEN COALESCE((
        SELECT event.payload->>'error' FROM events AS event
        WHERE event.tenant_id = run.tenant_id AND event.run_id = run.id
          AND event.type = 'run.state_changed' AND event.payload->>'to' = 'failed'
        ORDER BY event.id DESC LIMIT 1
    ), '') ELSE '' END,
    cancel_reason = CASE WHEN run.state = 'cancelled' THEN
        CASE WHEN EXISTS (
            SELECT 1 FROM run_approvals AS approval
            WHERE approval.tenant_id = run.tenant_id AND approval.run_id = run.id
              AND approval.decision = 'rejected'
        ) THEN 'rejected' ELSE 'cancelled' END
        ELSE '' END;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN cancel_reason, DROP COLUMN error, DROP COLUMN stop_reason;
-- +goose StatementEnd
