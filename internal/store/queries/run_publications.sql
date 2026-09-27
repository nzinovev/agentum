-- name: EnsurePublication :one
-- Insert the run's publication row in state pending. The UNIQUE (run_id)
-- index plus ON CONFLICT DO NOTHING is what makes a repeated request
-- idempotent: the caller learns "already exists" from the empty return
-- (sql.ErrNoRows) without a second round trip and falls back to
-- GetPublicationForRun. The target columns stay empty here — the destination
-- is derived at the first attempt and then frozen.
-- user_id is whose name the run was created under; the publication actor is
-- always the system, never a person.
INSERT INTO run_publications (tenant_id, user_id, run_id,
                              provider, remote_branch, published_commit, state)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')
ON CONFLICT (run_id) DO NOTHING
RETURNING *;

-- name: GetPublicationForRun :one
-- The run's publication row, or no rows when no publication exists. Serves
-- the read surface (GET .../publication, the final-review block) and the
-- publish handler's reloads.
SELECT * FROM run_publications
WHERE tenant_id = $1 AND run_id = $2;

-- name: RequestPublication :one
-- RequestPublication starts an explicit retry and invalidates older queued jobs.
-- A concurrent claim with a live lease refuses the request atomically.
UPDATE run_publications
SET state = 'pending', request_id = request_id + 1,
    lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
  AND (state <> 'publishing' OR lease_expires_at IS NULL OR lease_expires_at < now())
RETURNING *;

-- name: ClaimPublication :one
-- ClaimPublication leases pending work or an expired attempt for this request.
-- Recorded outcomes require RequestPublication before another claim. The
-- request id prevents older queued jobs from consuming that explicit retry.
-- The attempt number fences outcome writes even when a job owner is reused.
-- NULL leases are recoverable because no worker holds a valid lease then.
UPDATE run_publications
SET state = 'publishing',
    attempts = attempts + 1,
    lease_owner = $3,
    lease_expires_at = $4,
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
  AND request_id = $5
  AND (state = 'pending' OR (state = 'publishing'
       AND (lease_expires_at IS NULL OR lease_expires_at < now())))
RETURNING *;

-- name: RecordPublicationSuccess :one
-- Record a completed publication: the frozen target (written only when the
-- row does not carry one yet — the target is derived once and a later attempt
-- must not re-derive it against a moved remote), the pull request identity,
-- and both outcome marks. State published requires branch_pushed_at and
-- published_at together; the lease is released. Only the current attempt
-- and lease owner can record the outcome.
UPDATE run_publications
SET state = 'published',
    draft_rejected = false,
    target_host = COALESCE(NULLIF(target_host, ''), $3),
    target_owner = COALESCE(NULLIF(target_owner, ''), $4),
    target_repository = COALESCE(NULLIF(target_repository, ''), $5),
    base_branch = COALESCE(NULLIF(base_branch, ''), $6),
    pr_number = $7,
    pr_url = $8,
    pr_state = $9,
    branch_pushed_at = now(),
    published_at = now(),
    lease_owner = NULL,
    lease_expires_at = NULL,
    last_error_code = NULL,
    last_error_message = NULL,
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
  AND state = 'publishing' AND attempts = $10 AND lease_owner = $11
RETURNING *;

-- name: RecordPublicationFailure :one
-- Record a refused publication: the reason code from the closed vocabulary,
-- a safe diagnostic, and the state — failed when a retry can clear the
-- reason, blocked when it cannot. branch_pushed_at keeps an earlier
-- successful push: an attempt can push the branch and still fail the pull
-- request, and the row must keep the half that succeeded. The lease is
-- released. A draft refusal survives other failures until a successful delivery.
-- Only the current attempt and lease owner can record the outcome.
UPDATE run_publications
SET state = $3,
    draft_rejected = draft_rejected OR sqlc.arg(draft_rejected)::boolean,
    last_error_code = $4,
    last_error_message = $5,
    branch_pushed_at = COALESCE($6, branch_pushed_at),
    pr_number = COALESCE(sqlc.narg(pr_number)::integer, pr_number),
    pr_url = COALESCE(sqlc.narg(pr_url)::text, pr_url),
    pr_state = COALESCE(sqlc.narg(pr_state)::text, pr_state),
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
  AND state = 'publishing' AND attempts = $7 AND lease_owner = $8
RETURNING *;

-- name: FindStalePublications :many
-- Reconciler probe: publications the queue lost or a worker died on —
-- pending with no live publish job, or publishing with an expired lease.
-- Bounded by publication attempts and failed job attempts ($2, the poison
-- bound), including failures before a publication lease could be acquired.
-- Failed jobs count against their request only. Recorded failed and blocked
-- outcomes require an explicit retry.
SELECT publication.* FROM run_publications publication
WHERE publication.tenant_id = $1
  AND publication.attempts < $2
  AND (SELECT COALESCE(sum(GREATEST(failed_job.attempts, 1)), 0)
       FROM jobs failed_job
       WHERE failed_job.tenant_id = publication.tenant_id
         AND failed_job.run_id = publication.run_id
         AND failed_job.kind = 'publish' AND failed_job.status = 'failed'
         AND COALESCE(failed_job.payload->>'request_id', '0') = publication.request_id::text
      ) < $2
  AND (
      (publication.state = 'pending' AND NOT EXISTS (
          SELECT 1 FROM jobs liveJob
          WHERE liveJob.run_id = publication.run_id
            AND liveJob.tenant_id = publication.tenant_id
            AND liveJob.kind = 'publish'
            AND COALESCE(liveJob.payload->>'request_id', '0') = publication.request_id::text
            AND liveJob.status IN ('pending', 'running')
      ))
      OR (
          publication.state = 'publishing'
          AND (publication.lease_expires_at IS NULL OR publication.lease_expires_at < now())
      )
  )
ORDER BY publication.created_at;

-- name: FreezePublicationTarget :one
-- FreezePublicationTarget pins the destination before any network write.
-- A failed push or a crash after PR creation must retry against this destination.
UPDATE run_publications
SET target_host = COALESCE(NULLIF(target_host, ''), $3),
    target_owner = COALESCE(NULLIF(target_owner, ''), $4),
    target_repository = COALESCE(NULLIF(target_repository, ''), $5),
    base_branch = COALESCE(NULLIF(base_branch, ''), $6),
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2 AND user_id = $7
  AND state = 'publishing' AND attempts = $8 AND lease_owner = $9
  AND lease_expires_at > now()
RETURNING *;
