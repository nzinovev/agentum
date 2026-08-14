-- +goose Up
-- +goose StatementBegin

-- ADR 0003: durable human-approval records. One row per (tenant, task, name).
-- The unique index is what makes a repeated approve a no-op rather than a
-- second decision: CreateApproval uses ON CONFLICT DO NOTHING, and the handler
-- learns "already decided" from the empty return without a second round trip.
-- Two gates write here: the pack-declared approval name (e.g. "plan") at the
-- plan gate, and "final_review" at the final gate.
CREATE TABLE task_approvals (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            text NOT NULL,
    task_id              uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- The pack-declared approval name ("plan"), or the orchestrator-owned
    -- "final_review". One row per (task, name): the unique index below is what
    -- makes a repeated approve a no-op rather than a second decision.
    name                 text NOT NULL,
    decision             text NOT NULL,          -- approved | rejected
    -- The artifact revision the decision applies to. NULL for a gate that
    -- approves a run rather than a document (final_review).
    artifact_revision_id uuid NULL REFERENCES artifact_revisions(id),
    actor                text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_task_approvals_unique ON task_approvals(tenant_id, task_id, name);
CREATE INDEX idx_task_approvals_task ON task_approvals(task_id, tenant_id);

-- ADR 0003 D7: the final-review state is renamed. Pre-1.0, so the rows are
-- rewritten rather than dual-read.
UPDATE tasks SET state = 'awaiting_final_review' WHERE state = 'awaiting_memory_commit';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE tasks SET state = 'awaiting_memory_commit' WHERE state = 'awaiting_final_review';
DROP INDEX IF EXISTS idx_task_approvals_task;
DROP INDEX IF EXISTS idx_task_approvals_unique;
DROP TABLE IF EXISTS task_approvals;
-- +goose StatementEnd
