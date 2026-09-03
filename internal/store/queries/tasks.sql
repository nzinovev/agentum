-- name: CreateTask :one
INSERT INTO tasks (tenant_id, user_id, project_id, pipeline_pack,
                   title, description, overrides, base_ref, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'created')
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND tenant_id = $2;

-- name: ListTasksByProject :many
SELECT * FROM tasks
WHERE tenant_id = $1 AND project_id = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: UpdateTaskState :one
UPDATE tasks SET state = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: UpdateTaskStage :one
-- Set the runner's current position in the pack and (optionally) the state in
-- one write. currentStage may be empty (e.g. clearing on terminal); state is
-- always set. Used by the runner as it walks the pack's stages.
UPDATE tasks SET current_stage = $3, state = $4, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetBaseCommit :one
-- Resolve-once: capture the immutable SHA the task's base_ref pointed at. The
-- runner calls this before creating the worktree; the WHERE keeps it a no-op
-- after the first capture so the recorded base cannot drift if base_ref is
-- later moved. When the value is already pinned the UPDATE matches nothing
-- and returns NO row (sql.ErrNoRows): the caller must read that as "another
-- writer pinned first" and re-read the row, not as a failure.
UPDATE tasks SET base_commit = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND base_commit IS NULL
RETURNING *;

-- name: SetCheckoutPath :one
-- Resolve-once, like SetBaseCommit: the run pins the working copy it executes
-- in at first start and never re-resolves it, so re-registering the project
-- from another clone cannot pull an in-flight run into a foreign directory.
-- The empty string means "not pinned yet". When the copy is already pinned
-- the UPDATE matches nothing and returns NO row (sql.ErrNoRows): the caller
-- must read that as "another writer pinned first" and re-read the row, not as
-- a failure.
UPDATE tasks SET checkout_path = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND checkout_path = ''
RETURNING *;

-- name: RebindActiveCheckouts :many
-- The repository moved: the previous path no longer holds this repository, so
-- the working copy is one and it relocated. Only non-terminal runs — a
-- terminal run's checkout_path is a historical statement about where the work
-- happened, and rewriting it would falsify the record.
UPDATE tasks SET checkout_path = $4, updated_at = now()
WHERE tenant_id = $1 AND project_id = $2 AND checkout_path = $3
  AND state NOT IN ('done', 'failed', 'cancelled')
RETURNING *;

-- name: CountActiveTasksOnCheckout :one
-- The second working copy: how many unfinished runs stay in the previous one.
-- The registration response names this count so the split is visible at
-- registration time, not when someone tries to continue a run.
SELECT count(*) FROM tasks
WHERE tenant_id = $1 AND project_id = $2 AND checkout_path = $3
  AND state NOT IN ('done', 'failed', 'cancelled');

-- name: GetTaskForUpdate :one
-- Locking read used by SetBaseCommit callers that need the post-resolution row
-- even when the UPDATE matched zero rows (base_commit already set). FOR UPDATE
-- serializes concurrent first-resolvers on the same task.
SELECT * FROM tasks WHERE id = $1 AND tenant_id = $2 FOR UPDATE;

-- name: SetResultCommit :one
-- Capture the tip of agentum/<task-id> at final approval. The branch + this
-- commit remain resolvable after teardown; recorded once, on approve.
UPDATE tasks SET result_commit = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: FindOrphanedRunningTasks :many
-- Reconciler probe (F.6.1 AC #6): tasks whose state says they are running but
-- have no live (pending or running) job. A crash between the FSM transition and
-- EnqueueJob, or a worker that died and its job already failed, leaves the task
-- here. The reconciler transitions these to paused_user_stop (interrupted) so a
-- human explicitly resumes — safer than auto-replay of a half-run stage.
SELECT t.* FROM tasks t
WHERE t.tenant_id = $1
  AND t.state = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.task_id = t.id
        AND j.tenant_id = t.tenant_id
        AND j.status IN ('pending', 'running')
  )
ORDER BY t.updated_at;
