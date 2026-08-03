-- name: CreateArtifactRevision :one
-- Insert a new immutable revision. The caller (internal/artifacts.Service) is
-- responsible for writing the FS blob first and for the prev_revision_id chain
-- (it loads the prior current revision, passes its id here, then demotes it via
-- DemoteArtifactRevision). source_invocation_id is NULL for human-authored
-- edits. The execution coordinate (delivery_step / execution_unit / phase) is
-- optional and inert when NULL — a single-unit run never fills it.
INSERT INTO artifact_revisions (
    tenant_id, user_id, task_id,
    name, kind,
    content_hash, content_size,
    action_type, prev_revision_id,
    source_invocation_id,
    delivery_step, execution_unit, phase,
    actor,
    is_current
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, true)
RETURNING *;

-- name: DemoteArtifactRevisionIfCurrent :execrows
-- Flip one specific revision of (task_id, name) from current to superseded.
-- Pinning the id (rather than "whatever is current") is what makes the write
-- safe under concurrency: the caller read that id inside this transaction, so
-- an affected-row count of 0 means another writer moved the chain in between
-- and the caller must abort with ErrRevisionConflict rather than chain onto a
-- revision that is no longer current.
--
-- Runs in the same transaction as CreateArtifactRevision so the
-- idx_artifact_rev_current unique partial index never sees two currents at once.
UPDATE artifact_revisions SET is_current = false
WHERE task_id = $1 AND name = $2 AND id = $3 AND is_current = true;

-- name: GetArtifactRevision :one
SELECT * FROM artifact_revisions WHERE id = $1 AND tenant_id = $2;

-- name: CurrentArtifactRevisionForName :one
-- The revision a resume / next stage syncs into the worktree for (task_id,
-- name). Backed by the partial unique index idx_artifact_rev_current; one row
-- max. Returns no rows when the task has no revision of that name yet.
SELECT * FROM artifact_revisions
WHERE task_id = $1 AND tenant_id = $2 AND name = $3 AND is_current = true;

-- name: LockCurrentArtifactRevisionForName :one
-- The same lookup as CurrentArtifactRevisionForName, but taking a row lock so
-- the read-decide-write sequence in artifacts.SQLStore.Put is serialized
-- against concurrent writers to the same (task_id, name). Must be called inside
-- a transaction; a second writer blocks here until the first commits and then
-- observes the new current revision.
--
-- No row to lock means no current revision, which is the "first create" case:
-- two racing first-creates are caught instead by the idx_artifact_rev_current
-- unique partial index, and the loser's insert fails.
SELECT * FROM artifact_revisions
WHERE task_id = $1 AND tenant_id = $2 AND name = $3 AND is_current = true
FOR UPDATE;

-- name: ListArtifactRevisionsForTask :many
-- All revisions for a task, newest first. Includes prior, non-current
-- revisions so the audit trail reads in full.
SELECT * FROM artifact_revisions
WHERE task_id = $1 AND tenant_id = $2
ORDER BY name ASC, created_at DESC;

-- name: ListArtifactRevisionsForInvocation :many
-- Revisions produced by a specific stage invocation. Backed by the partial
-- index on source_invocation_id.
SELECT * FROM artifact_revisions
WHERE source_invocation_id = $1 AND tenant_id = $2
ORDER BY created_at ASC;

-- name: ListCurrentArtifactRevisionsForTask :many
-- The current revisions only — the snapshot a resume / comparison reads.
SELECT * FROM artifact_revisions
WHERE task_id = $1 AND tenant_id = $2 AND is_current = true
ORDER BY name ASC;
