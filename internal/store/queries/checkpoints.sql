-- name: CreateCheckpoint :one
-- Upsert a boundary checkpoint (F.6.1 AC #1, #2). The orchestrator owns these;
-- agents cannot. (task_id, label) is unique, so a retry after a crash that
-- re-hits the same boundary replaces the SHA rather than duplicating. commit_sha
-- is the immutable full SHA the worktree HEAD pointed at when the boundary was
-- crossed.
INSERT INTO task_checkpoints (tenant_id, user_id, task_id, label, commit_sha)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (task_id, label) DO UPDATE SET
    commit_sha = EXCLUDED.commit_sha,
    created_at = now()
RETURNING *;

-- name: LatestCheckpointForTask :one
-- The most recent checkpoint for a task — the restore target when the reconciler
-- classifies a crashed worktree as restorable. Returns no rows if the task has
-- no checkpoints yet (the reconciler then falls back to base_commit or surfaces
-- for human attention).
SELECT * FROM task_checkpoints
WHERE task_id = $1 AND tenant_id = $2
ORDER BY created_at DESC
LIMIT 1;

-- name: ListCheckpointsForTask :many
-- Ordered oldest-first so the audit trail reads in execution order.
SELECT * FROM task_checkpoints
WHERE task_id = $1 AND tenant_id = $2
ORDER BY created_at;
