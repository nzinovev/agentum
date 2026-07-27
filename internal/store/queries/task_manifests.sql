-- name: InitManifest :one
-- Create the per-task manifest row. UNIQUE (task_id) makes a second Init a
-- constraint violation — callers treat that as "already initialized" and move
-- on to AddEvidence. The body starts empty; sections are filled by
-- AddEvidence as evidence accumulates.
INSERT INTO task_manifests (tenant_id, user_id, task_id, body)
VALUES ($1, $2, $3, $4)
ON CONFLICT (task_id) DO NOTHING
RETURNING *;

-- name: GetManifest :one
SELECT * FROM task_manifests WHERE task_id = $1 AND tenant_id = $2;

-- name: GetManifestForUpdate :one
-- Locking read used by AddEvidence / SealManifest so two concurrent writers
-- cannot interleave section writes. FOR UPDATE serializes them on the row.
SELECT * FROM task_manifests WHERE task_id = $1 AND tenant_id = $2 FOR UPDATE;

-- name: AddManifestEvidence :one
-- Merge a JSONB patch into the body. Refuses to write when the manifest is
-- already sealed — sealed_at IS NOT NULL means the body is immutable and any
-- correction must go through task_manifest_corrections. Returns the updated
-- row so the caller sees the post-merge body.
--
-- The merge is shallow at the SQL level (||); the Go service is responsible
-- for constructing a patch that respects append-only semantics per section
-- (e.g. it appends to the `human_decisions` array rather than replacing it).
UPDATE task_manifests
SET body = body || $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND sealed_at IS NULL
RETURNING *;

-- name: SealManifest :one
-- Set sealed_at + seal_reason + sealed_by. WHERE sealed_at IS NULL makes a
-- second Seal a no-op; the caller re-reads to get the canonical row.
UPDATE task_manifests
SET sealed_at = now(), sealed_by = $3, seal_reason = $4, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND sealed_at IS NULL
RETURNING *;

-- name: CreateManifestCorrection :one
-- A post-seal amendment. The body is a full corrected manifest snapshot; the
-- sealed row is never edited. `reason` explains why the correction was made.
-- Consumers read the sealed row + its corrections ordered by created_at.
INSERT INTO task_manifest_corrections (tenant_id, user_id, manifest_id, body, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListManifestCorrections :many
SELECT * FROM task_manifest_corrections
WHERE manifest_id = $1 AND tenant_id = $2
ORDER BY created_at ASC;
