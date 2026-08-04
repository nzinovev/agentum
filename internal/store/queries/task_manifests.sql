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
-- Replace the body with an already-merged body the caller computed. Refuses to
-- write when the manifest is already sealed — sealed_at IS NOT NULL means the
-- body is immutable and any correction must go through task_manifest_corrections.
-- Returns the updated row.
--
-- This is a full replacement, not a JSONB merge, on purpose: the caller holds
-- the row lock (GetManifestForUpdate) and has already merged the patch into the
-- locked body in Go. A SQL-level `||` merge here would be a second, shallow
-- merge whose top-level-key-only semantics silently drop nested evidence a
-- concurrent writer just committed — the exact lost update the row lock exists
-- to prevent. Replacing the whole body keeps the merge logic in one place (the
-- Go merge functions) and makes it deep by construction.
UPDATE task_manifests
SET body = $3, updated_at = now()
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
ORDER BY created_at ASC, id ASC;

-- name: LatestManifestCorrection :one
-- The newest correction for a manifest, or no rows when none exist. Ordered by
-- created_at DESC with an id DESC tiebreak: created_at has limited resolution
-- (microseconds on Postgres), so two corrections written in the same transaction
-- could otherwise order nondeterministically, and the latest one is the base the
-- next correction chains onto — ordering must be stable. ListManifestCorrections
-- mirrors this tiebreak (ASC, id ASC) so the two queries agree on order and Get,
-- which takes the last row as the authoritative body, sees the true chain head.
SELECT * FROM task_manifest_corrections
WHERE manifest_id = $1 AND tenant_id = $2
ORDER BY created_at DESC, id DESC
LIMIT 1;
