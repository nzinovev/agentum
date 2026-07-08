-- +goose Up
-- +goose StatementBegin

-- tasks.current_stage: where the runner is in the pack's stage graph. Nullable
-- before start; set each time the runner begins a stage. Combined with
-- stage_invocations.sequence this gives a deterministic resume point and a
-- single-row UI read for "where is this task" (04 §7.1.7).
ALTER TABLE tasks ADD COLUMN current_stage text;

-- The job queue (04 §7.5). HTTP handlers enqueue a job and return; a worker
-- claims and drives it. Postgres-backed via FOR UPDATE SKIP LOCKED — no Redis,
-- transactional with task state. payload carries continue/advance inputs
-- (edits, context, answers) so the worker has everything it needs at claim time.
CREATE TABLE jobs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    uuid NOT NULL,
    user_id      uuid NOT NULL,
    task_id      uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind         text NOT NULL,                       -- run | continue | advance | cancel
    status       text NOT NULL DEFAULT 'pending',     -- pending | running | done | failed
    worker_id    text,                                -- who claimed it (hostname:pid)
    heartbeat_at timestamptz,                         -- bumped by the worker during a run
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text,
    payload      jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);

-- Partial index: only pending rows are claimable, so the claim scan stays tiny
-- regardless of how many done/failed rows accumulate.
CREATE INDEX idx_jobs_claim ON jobs(id) WHERE status = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS jobs;
ALTER TABLE tasks DROP COLUMN IF EXISTS current_stage;

-- +goose StatementEnd
