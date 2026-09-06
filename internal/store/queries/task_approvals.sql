-- name: CreateApproval :one
-- Insert one human-approval decision. The UNIQUE (tenant_id, task_id, name)
-- index plus ON CONFLICT DO NOTHING is what makes a repeated approve idempotent:
-- the handler learns "already decided" from the empty return (sql.ErrNoRows)
-- without a second round trip, and a different decision on an already-decided
-- gate is surfaced by GetApproval and stays a 409 at the handler. Returns no
-- rows on conflict — the caller falls back to GetApproval to read the existing
-- decision.
-- user_id is whose name the decision was recorded under; actor is who acted
-- ('human' for every writer today — the shared human|agent|system vocabulary).
INSERT INTO task_approvals (tenant_id, user_id, task_id, name, decision, artifact_revision_id, actor)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, task_id, name) DO NOTHING
RETURNING *;

-- name: GetApproval :one
-- Read the decision row for one (task, name), or no rows when undecided. Used
-- by the runner to decide whether the source_write withholding applies, and by
-- the handler to implement idempotent approve/reject.
SELECT * FROM task_approvals
WHERE tenant_id = $1 AND task_id = $2 AND name = $3;

-- name: ListApprovalsForTask :many
-- Every decision recorded for a task, oldest first. Feeds the final-review
-- payload and the manifest's gate_decisions audit section.
SELECT * FROM task_approvals
WHERE tenant_id = $1 AND task_id = $2
ORDER BY created_at ASC, id ASC;
