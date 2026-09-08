-- name: CreateRun :one
INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
                  title, description, overrides, base_ref, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'created')
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1 AND tenant_id = $2;

-- name: ListRunsByProject :many
SELECT * FROM runs
WHERE tenant_id = $1 AND project_id = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: UpdateRunState :one
UPDATE runs SET state = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: UpdateRunStage :one
-- Set the runner's current position in the pack and (optionally) the state in
-- one write. currentStage may be empty (e.g. clearing on terminal); state is
-- always set. Used by the runner as it walks the pack's stages.
UPDATE runs SET current_stage = $3, state = $4, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetBaseCommit :one
-- Resolve-once: capture the immutable SHA the run's base_ref pointed at. The
-- runner calls this before creating the worktree; the WHERE keeps it a no-op
-- after the first capture so the recorded base cannot drift if base_ref is
-- later moved. When the value is already pinned the UPDATE matches nothing
-- and returns NO row (sql.ErrNoRows): the caller must read that as "another
-- writer pinned first" and re-read the row, not as a failure.
UPDATE runs SET base_commit = $3, updated_at = now()
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
UPDATE runs SET checkout_path = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND checkout_path = ''
RETURNING *;

-- name: RebindActiveCheckouts :many
-- The repository moved: the previous path no longer holds this repository, so
-- the working copy is one and it relocated. Only non-terminal runs — a
-- terminal run's checkout_path is a historical statement about where the work
-- happened, and rewriting it would falsify the record.
UPDATE runs SET checkout_path = $4, updated_at = now()
WHERE tenant_id = $1 AND project_id = $2 AND checkout_path = $3
  AND state NOT IN ('done', 'failed', 'cancelled')
RETURNING *;

-- name: CountActiveRunsOnCheckout :one
-- The second working copy: how many unfinished runs stay in the previous one.
-- The registration response names this count so the split is visible at
-- registration time, not when someone tries to continue a run.
SELECT count(*) FROM runs
WHERE tenant_id = $1 AND project_id = $2 AND checkout_path = $3
  AND state NOT IN ('done', 'failed', 'cancelled');

-- name: GetRunForUpdate :one
-- Locking read used by SetBaseCommit callers that need the post-resolution row
-- even when the UPDATE matched zero rows (base_commit already set). FOR UPDATE
-- serializes concurrent first-resolvers on the same run.
SELECT * FROM runs WHERE id = $1 AND tenant_id = $2 FOR UPDATE;

-- name: SetResultCommit :one
-- Capture the tip of agentum/<run-id> at final approval. The branch + this
-- commit remain resolvable after teardown; recorded once, on approve.
UPDATE runs SET result_commit = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: FindOrphanedRunningRuns :many
-- Reconciler probe (F.6.1 AC #6): runs whose state says they are running but
-- have no live (pending or running) job. A crash between the FSM transition and
-- EnqueueJob, or a worker that died and its job already failed, leaves the run
-- here. The reconciler transitions these to paused_user_stop (interrupted) so a
-- human explicitly resumes — safer than auto-replay of a half-run stage.
SELECT runningRun.* FROM runs runningRun
WHERE runningRun.tenant_id = $1
  AND runningRun.state = 'running'
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.run_id = runningRun.id
        AND j.tenant_id = runningRun.tenant_id
        AND j.status IN ('pending', 'running')
  )
ORDER BY runningRun.updated_at;
