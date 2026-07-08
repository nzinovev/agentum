-- name: CreateTask :one
INSERT INTO tasks (tenant_id, user_id, project_id, pipeline_pack, title, input, state)
VALUES ($1, $2, $3, $4, $5, $6, 'created')
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
