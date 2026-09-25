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

-- name: ClaimPublication :one
-- Lease the publication for one attempt: state moves to publishing, attempts
-- increments, and the owner plus expiry are recorded. The WHERE clause is
-- mutual exclusion by lease, not by row lock — the network call that follows
-- must not run inside a transaction, so two workers racing here resolve by
-- the conditional UPDATE, and the loser reads no row. A publishing row whose
-- lease has expired is claimable again: its worker died mid-attempt.
UPDATE run_publications
SET state = 'publishing',
    attempts = attempts + 1,
    lease_owner = $3,
    lease_expires_at = $4,
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
  AND (state <> 'publishing' OR lease_expires_at < now())
RETURNING *;

-- name: RecordPublicationSuccess :one
-- Record a completed publication: the frozen target (written only when the
-- row does not carry one yet — the target is derived once and a later attempt
-- must not re-derive it against a moved remote), the pull request identity,
-- and both outcome marks. State published requires branch_pushed_at and
-- published_at together; the lease is released.
UPDATE run_publications
SET state = 'published',
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
RETURNING *;

-- name: RecordPublicationFailure :one
-- Record a refused publication: the reason code from the closed vocabulary,
-- the provider's message, and the state — failed when a retry can clear the
-- reason, blocked when it cannot. branch_pushed_at keeps an earlier
-- successful push: an attempt can push the branch and still fail the pull
-- request, and the row must keep the half that succeeded. The lease is
-- released.
UPDATE run_publications
SET state = $3,
    last_error_code = $4,
    last_error_message = $5,
    branch_pushed_at = COALESCE($6, branch_pushed_at),
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE tenant_id = $1 AND run_id = $2
RETURNING *;

-- name: FindStalePublications :many
-- Reconciler probe: publications the queue lost or a worker died on —
-- pending with no live publish job, or publishing with an expired lease.
-- Bounded by attempts ($2, the job poison bound) so recovery cannot loop
-- forever. failed and blocked rows are never returned: the system restores
-- work it did not finish, it does not retry an outcome it recorded.
SELECT publication.* FROM run_publications publication
WHERE publication.tenant_id = $1
  AND publication.attempts < $2
  AND (
      (publication.state = 'pending' AND NOT EXISTS (
          SELECT 1 FROM jobs liveJob
          WHERE liveJob.run_id = publication.run_id
            AND liveJob.tenant_id = publication.tenant_id
            AND liveJob.kind = 'publish'
            AND liveJob.status IN ('pending', 'running')
      ))
      OR (
          publication.state = 'publishing'
          AND (publication.lease_expires_at IS NULL OR publication.lease_expires_at < now())
      )
  )
ORDER BY publication.created_at;
