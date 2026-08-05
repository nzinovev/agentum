-- +goose Up
-- +goose StatementBegin

-- ADR 0001 — durable fix-cycle budget.
--
-- stage_invocations.cycle is the 0-based repeat index of that stage within the
-- task: the first entry into a stage is cycle 0, a later fresh entry is
-- MAX(cycle for (task, stage)) + 1, and a `continue` resume inherits the
-- resumed invocation's cycle unchanged. 0 is therefore a legitimate value
-- (every stage's first invocation carries it), not a gap.
--
-- The runner derives fix_cycles_used = MAX(cycle) + 1 over every fixer-role
-- stage and refuses the next fixer entry once it reaches budgets.fix_cycles.
-- Deriving the counter from committed rows (rather than a separate counter
-- column) makes it durable by construction: it survives a worker restart
-- because it is recomputed from history, it cannot be inflated by a resume
-- because a resume inherits its cycle, and it is idempotent under a crash
-- between "transition chosen" and "invocation created" (no row was written, so
-- the recomputed value is identical).
--
-- NOT NULL DEFAULT 0 so existing rows (stages that ran before this migration)
-- and a fresh first invocation both carry the legitimate "first entry" value
-- rather than a NULL that the runner would have to treat as a special case.

ALTER TABLE stage_invocations ADD COLUMN cycle integer NOT NULL DEFAULT 0;

-- Index supporting MaxCycleForStages (the budget counter): a per-(task, stage)
-- max over a small set of fixer-role stages. cycle DESC lets the index satisfy
-- "the highest cycle for this stage" as a prefix scan.
CREATE INDEX idx_stage_invocations_stage_cycle ON stage_invocations(task_id, stage, cycle DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_stage_invocations_stage_cycle;
ALTER TABLE stage_invocations DROP COLUMN IF EXISTS cycle;

-- +goose StatementEnd
