-- name: ListEventsAfter :many
-- Tenant-global tail: events with id > afterID, ordered for SSE replay / live
-- tail. Used by GET /api/v1/events. Last-Event-ID maps to afterID.
SELECT * FROM events
WHERE tenant_id = $1 AND id > $2
ORDER BY id ASC
LIMIT $3;

-- name: ListEventsAfterTask :many
-- Per-task tail: same shape, scoped to one task. Used by GET /tasks/{id}/events.
SELECT * FROM events
WHERE tenant_id = $1 AND task_id = $2 AND id > $3
ORDER BY id ASC
LIMIT $4;

-- name: AppendEvent :one
-- Insert a row in the durable event log. The runner is the only caller in
-- production; tests and the SSE handler read. id is monotonic, so Last-Event-ID
-- is a simple "> last_id" tail.
INSERT INTO events (tenant_id, user_id, task_id, type, payload)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;
