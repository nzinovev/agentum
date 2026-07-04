-- name: CreateStageInvocation :one
INSERT INTO stage_invocations (tenant_id, user_id, task_id, stage, sequence, session_id, resume_of, stop_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetStageInvocation :one
SELECT * FROM stage_invocations WHERE id = $1 AND tenant_id = $2;

-- name: LatestStageForTask :one
SELECT * FROM stage_invocations
WHERE task_id = $1 AND tenant_id = $2
ORDER BY sequence DESC
LIMIT 1;

-- name: SetStageSession :exec
UPDATE stage_invocations SET session_id = $3
WHERE id = $1 AND tenant_id = $2;

-- name: SetStageStop :exec
UPDATE stage_invocations
SET stop_reason = $3, finished_at = now()
WHERE id = $1 AND tenant_id = $2;
