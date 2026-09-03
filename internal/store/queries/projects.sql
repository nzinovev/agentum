-- name: CreateProject :one
-- Idempotent registration keyed on the repository's identity, not its path:
-- the same working copy (or a clone of the same history) under a new path
-- updates the path and stays the same project, so a moved directory keeps its
-- run history, memory and approvals. repo_identity is computed at the
-- boundary (internal/repoid) and never accepted from a request body.
-- Callers must pass a non-nil related_projects slice (the column is NOT
-- NULL; pq.Array(nil) is NULL).
INSERT INTO projects (tenant_id, user_id, repo_identity, repo_path, name, related_projects)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, repo_identity) DO UPDATE SET
    repo_path = EXCLUDED.repo_path,
    name = EXCLUDED.name,
    related_projects = EXCLUDED.related_projects,
    updated_at = now()
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1 AND tenant_id = $2;

-- name: GetProjectByIdentity :one
-- The pre-upsert read of the registration flow: the handler must know the
-- project's previous repo_path (which the upsert overwrites) to resolve it
-- and decide whether unfinished runs stay in the old copy or move with it.
SELECT * FROM projects WHERE tenant_id = $1 AND repo_identity = $2;

-- name: ListProjects :many
SELECT * FROM projects
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
