-- name: CreateProject :one
-- Idempotent registration: one repo_path = one project per tenant. Re-registering
-- the same repo touches updated_at and applies the new fields (name, related set)
-- rather than failing, so onboarding scripts are restart-safe.
INSERT INTO projects (tenant_id, user_id, repo_path, name, related_projects)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, repo_path) DO UPDATE SET
    name = EXCLUDED.name,
    related_projects = EXCLUDED.related_projects,
    updated_at = now()
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1 AND tenant_id = $2;

-- name: GetProjectByRepoPath :one
-- Lookup by repo_path within a tenant (used to resolve a project from a path
-- without a round-trip of list-then-match).
SELECT * FROM projects WHERE tenant_id = $1 AND repo_path = $2;

-- name: ListProjects :many
SELECT * FROM projects
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
