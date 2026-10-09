-- name: CreateRun :one
INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
                  title, description, overrides, base_ref, state, route_source, route_reason, route_decided_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'created', NULLIF(sqlc.arg(route_source)::text, ''),
        CASE WHEN sqlc.arg(route_source)::text = 'request' THEN 'pipeline_pack was set in POST /runs. Triage did not run for this run.' ELSE '' END,
        CASE WHEN sqlc.arg(route_source)::text = 'request' THEN now() ELSE NULL END)
RETURNING *;

-- name: SelectRunRoute :one
UPDATE runs SET pipeline_pack = $3, route_source = $4, route_reason = $5,
    route_fallback_code = $6, route_fallback_message = $7,
    route_triage_invocation_id = $8, route_decided_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND route_source IS NULL AND state IN ('created', 'running')
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1 AND tenant_id = $2;

-- name: ListRunsByProject :many
SELECT * FROM runs
WHERE tenant_id = $1 AND project_id = $2
ORDER BY CASE WHEN state IN ('paused_open_questions', 'paused_gate', 'paused_user_stop', 'awaiting_final_review') THEN 0 ELSE 1 END,
         updated_at DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: UpdateRunState :one
UPDATE runs SET state = $3,
    stop_reason = CASE WHEN $3::text IN ('paused_open_questions', 'paused_gate', 'paused_user_stop') THEN sqlc.arg(stop_reason)::text ELSE '' END,
    error = CASE WHEN $3::text = 'failed' THEN sqlc.arg(error)::text ELSE '' END,
    cancel_reason = CASE WHEN $3::text = 'cancelled' THEN sqlc.arg(cancel_reason)::text ELSE '' END,
    pause_requested_at = CASE WHEN $3::text = 'running' THEN pause_requested_at ELSE NULL END,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: UpdateRunStage :one
-- Set the runner's current position in the pack and (optionally) the state in
-- one write. currentStage may be empty (e.g. clearing on terminal); state is
-- always set. Used by the runner as it walks the pack's stages.
UPDATE runs SET current_stage = $3, state = $4,
    stop_reason = CASE WHEN $4::text IN ('paused_open_questions', 'paused_gate', 'paused_user_stop') THEN sqlc.arg(stop_reason)::text ELSE '' END,
    error = '', cancel_reason = '',
    pause_requested_at = CASE WHEN $4::text = 'running' THEN pause_requested_at ELSE NULL END,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: RequestRunPause :one
-- A repeated request preserves the first timestamp while the runner finishes
-- its current invocation and checkpoint.
UPDATE runs SET pause_requested_at = COALESCE(pause_requested_at, now()), updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND state = 'running'
RETURNING *;

-- name: ReopenPlanGate :one
-- ReopenPlanGate stores the FSM result with the artifact edit in one transaction.
-- The plan revision and the pause cannot become visible separately.
UPDATE runs SET state = sqlc.arg(next_state), current_stage = $3,
    stop_reason = 'plan_revision_drift', pause_requested_at = NULL,
    active_fix_request_revision_id = NULL,
    previous_result_commit = COALESCE(result_commit, previous_result_commit),
    result_commit = NULL, updated_at = now()
WHERE id = $1 AND tenant_id = $2
  AND state IN ('paused_gate', 'paused_open_questions', 'paused_user_stop', 'awaiting_final_review')
RETURNING *;

-- name: ReopenFinalReviewForFix :one
-- ReopenFinalReviewForFix enters the FSM result only when the accepted human
-- revision is still current for the same reviewed commit.
UPDATE runs SET state = sqlc.arg(next_state), current_stage = $3,
    previous_result_commit = result_commit, result_commit = NULL,
    active_fix_request_revision_id = sqlc.arg(fix_revision_id),
    stop_reason = '', pause_requested_at = NULL, updated_at = now()
WHERE runs.id = $1 AND runs.tenant_id = $2 AND runs.state = 'awaiting_final_review'
  AND runs.result_commit = $4
  AND EXISTS (SELECT 1 FROM artifact_revisions
              WHERE artifact_revisions.run_id = runs.id AND artifact_revisions.tenant_id = runs.tenant_id
                AND artifact_revisions.name = 'final/fix-request.md'
                AND artifact_revisions.id = sqlc.arg(fix_revision_id)
                AND artifact_revisions.is_current = true)
RETURNING *;

-- name: SetActiveFixRequestRevision :one
-- Bind a retry's new human feedback revision in the same transaction that
-- resumes the run, so the fixer and reviewer read the accepted comment.
UPDATE runs SET active_fix_request_revision_id = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND state = 'running'
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

-- name: SetPipelinePackOrigin :one
-- Resolve-once, like SetBaseCommit: the run records where its pack's bytes
-- came from (builtin / project / project+builtin) at the first effective
-- resolution. The WHERE keeps it a no-op afterwards, so a later commit that
-- changes the project's packs cannot rewrite what a running record says it
-- executed. When the origin is already pinned the UPDATE matches nothing and
-- returns NO row (sql.ErrNoRows): the caller reads that as "already pinned".
UPDATE runs SET pipeline_pack_origin = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND pipeline_pack_origin IS NULL
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
