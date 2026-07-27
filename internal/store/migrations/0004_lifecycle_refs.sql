-- +goose Up
-- +goose StatementBegin

-- F.6.1 — safe lifecycle, code egress, checkpoint primitive.
--
-- The task now records the git base it was built against and the immutable
-- result commit at final approval, so delivery survives worktree teardown. The
-- worktree is disposable; the branch + commits are the durable delivery output.
-- Checkpoints are orchestrator-owned immutable SHAs at stage boundaries; on
-- retry/resume after a crash the reconciler restores the worktree to the last
-- checkpoint rather than blindly replaying a side-effectful stage.

-- base_ref: the user-supplied ref the task builds against (branch / tag / SHA).
--           Default 'HEAD' is resolved at first run. Stored verbatim so the
--           input record is reproducible.
-- base_commit: the full SHA base_ref resolved to, captured ONCE before the
--              worktree is created. Immutable after capture — checkpoints and
--              the final result_commit diff against this.
-- result_commit: the full SHA captured at final approval (the tip of
--                agentum/<task-id> at done). Remains resolvable after teardown.
ALTER TABLE tasks
    ADD COLUMN base_ref      text,
    ADD COLUMN base_commit   text,
    ADD COLUMN result_commit text;

-- tasks.base_ref defaults to 'HEAD' so a task created without an explicit ref
-- builds against the repo's current HEAD (the pre-F.6.1 behavior).
UPDATE tasks SET base_ref = 'HEAD' WHERE base_ref IS NULL;
ALTER TABLE tasks ALTER COLUMN base_ref SET DEFAULT 'HEAD';
ALTER TABLE tasks ALTER COLUMN base_ref SET NOT NULL;

-- Orchestrator-owned checkpoints (F.6.1 AC #1, #2). One row per (task, label):
-- the runner creates a checkpoint at stage boundaries (e.g. "base", "post-spec",
-- "pre-approve"). The label is the human/operor-facing name; commit_sha is the
-- immutable full SHA the worktree HEAD pointed at when the checkpoint was taken.
-- UNIQUE (task_id, label) makes CreateCheckpoint idempotent per label — the
-- runner upserts so a retry after crash records the same boundary without
-- duplicating.
CREATE TABLE task_checkpoints (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL,
    user_id     uuid NOT NULL,
    task_id     uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    label       text NOT NULL,
    commit_sha  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, label)
);

CREATE INDEX idx_checkpoints_task ON task_checkpoints(task_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS task_checkpoints;
ALTER TABLE tasks
    DROP COLUMN IF EXISTS result_commit,
    DROP COLUMN IF EXISTS base_commit,
    DROP COLUMN IF EXISTS base_ref;

-- +goose StatementEnd
