-- name: EnqueueJob :one
-- Insert a pending job. Called by HTTP handlers transactionally with the FSM
-- transition. kind ∈ {run, continue, advance, cancel}; payload carries the
-- inputs the worker needs (edits/context/answers).
INSERT INTO jobs (tenant_id, user_id, task_id, kind, payload)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ClaimNextJob :one
-- Atomically claim the oldest pending job for worker_id. FOR UPDATE SKIP LOCKED
-- lets multiple workers poll concurrently without blocking each other; each
-- skips rows another worker holds. attempts increments on every claim so the
-- poison-job bound (config.JobMaxAttempts, 04 §7.5) can cap retries.
UPDATE jobs
SET status = 'running', worker_id = $1, heartbeat_at = now(), attempts = attempts + 1
WHERE id = (
    SELECT id FROM jobs
    WHERE status = 'pending'
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: CompleteJob :exec
UPDATE jobs SET status = 'done', finished_at = now()
WHERE id = $1;

-- name: FailJob :exec
-- Mark a job failed with the reason. The recovery pass (or the caller) decides
-- whether the task moves to a paused state; this only records the job outcome.
UPDATE jobs SET status = 'failed', last_error = $2, finished_at = now()
WHERE id = $1;

-- name: BumpHeartbeat :exec
-- Refreshed by the worker during a run so the recovery pass can detect a dead
-- worker (heartbeat older than the stale threshold).
UPDATE jobs SET heartbeat_at = now()
WHERE id = $1 AND status = 'running';

-- name: RequeueStaleJobs :many
-- Recovery: re-queue jobs a worker died mid-run on (heartbeat older than the
-- threshold). Returns the re-queued rows so the caller can log / emit events.
-- The poison bound is enforced by the caller via attempts.
UPDATE jobs
SET status = 'pending', worker_id = NULL
WHERE id IN (
    SELECT j.id FROM jobs j
    WHERE j.status = 'running' AND j.heartbeat_at < $1
)
RETURNING *;

-- name: CountRunningJobsForTask :one
-- Belt-and-suspenders: how many running jobs a task has. Used to guard against
-- double-enqueue races (the FSM is the primary guard).
SELECT count(*)::int FROM jobs WHERE task_id = $1 AND status = 'running';
