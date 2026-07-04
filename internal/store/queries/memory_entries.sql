-- name: CommitMemoryEntry :one
-- Insert only at task-done / final approval. The producing agent emitted these
-- via memory_writes in result.json; they commit here.
INSERT INTO memory_entries (
    tenant_id, user_id, project_id, scope, kind, title, body, keywords, source_task_id, source_stage
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: RecentMemoryByProject :many
-- The push path: most-recent-N decisions, recency-ordered. The baseline flywheel
-- signal; zero agent cooperation required.
SELECT * FROM memory_entries
WHERE project_id = $1 AND scope = 'project' AND status = 'active'
ORDER BY created_at DESC
LIMIT $2;

-- name: SearchMemoryByKeyword :many
-- The pull path: the named handle, recency-ordered. Must land before the
-- flywheel test window closes, so relevance has a path and recency-only push
-- doesn't false-negative the thesis.
SELECT * FROM memory_entries
WHERE project_id = $1 AND scope = 'project' AND status = 'active'
  AND (title ILIKE '%' || $2 || '%' OR $2 = ANY(keywords) OR body ILIKE '%' || $2 || '%')
ORDER BY created_at DESC
LIMIT $3;
