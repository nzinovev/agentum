-- name: CreateStageInvocation :one
INSERT INTO stage_invocations (tenant_id, user_id, task_id, stage, sequence, session_id, resume_of, stop_reason, capability_profile, cycle)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetStageInvocation :one
SELECT * FROM stage_invocations WHERE id = $1 AND tenant_id = $2;

-- name: LatestStageForTask :one
SELECT * FROM stage_invocations
WHERE task_id = $1 AND tenant_id = $2
ORDER BY sequence DESC
LIMIT 1;

-- name: ListStageInvocationsForTask :many
-- Ordered by sequence so each attempt is visible in run order; the cycle column
-- distinguishes retries from resumes. Backs GET /tasks/{id}/invocations.
SELECT * FROM stage_invocations
WHERE task_id = $1 AND tenant_id = $2
ORDER BY sequence ASC;

-- name: MaxCycleForStages :one
-- The durable fix-cycle counter: the highest cycle any of the given (fixer-
-- role) stages has reached for this task. Returns -1 when none of the stages
-- has run yet (MAX over zero rows is NULL; COALESCE to a sentinel that cannot
-- be a real cycle, since cycles are >= 0). The runner maps -1 -> 0 entries.
-- stage = ANY($3) takes the fixer set in one round-trip (pq.Array over pgx
-- stdlib, already exercised by projects.related_projects).
SELECT COALESCE(MAX(cycle), -1)::int FROM stage_invocations
WHERE task_id = $1 AND tenant_id = $2 AND stage = ANY($3::text[]);

-- name: SetStageSession :exec
UPDATE stage_invocations SET session_id = $3
WHERE id = $1 AND tenant_id = $2;

-- name: SetStageStop :exec
UPDATE stage_invocations
SET stop_reason = $3, finished_at = now()
WHERE id = $1 AND tenant_id = $2;

-- name: FinishStageInvocation :exec
-- Record the parsed result.json + session_id + stop_reason on close. The runner
-- calls this after the adapter returns. result is the parsed result.json (the
-- file-derived fields); telemetry/session are stream-derived.
UPDATE stage_invocations
SET session_id = $3, stop_reason = $4, result = $5, finished_at = now()
WHERE id = $1 AND tenant_id = $2;
