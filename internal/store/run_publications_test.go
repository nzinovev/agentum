package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The publication row's contract as a schema property: one row per run, a
// lease that only one worker holds, and a recovery probe that returns lost
// work without touching recorded outcomes. These run against a real Postgres
// (dbtest) because the guarantees under test are exactly the database's —
// ON CONFLICT DO NOTHING and a conditional UPDATE — and a fake would
// re-implement them from the same reading this file is meant to check.

const (
	publicationTestTenantID = "3ad2b8e5-1f4a-4c0e-9d2b-6f7c8a9b0c1d"
	publicationTestUserID   = "4be3c9f6-2a5b-4d1f-8e3c-7a8d9b0c1e2f"
)

// insertPublicationFixture creates the project and run a publication row
// hangs off, and returns the run id. Raw inserts, not the registration flow:
// these tests exercise the publication queries, and the fixture's job is to
// exist, not to be interesting.
func insertPublicationFixture(t *testing.T, db *sql.DB, marker string) string {
	t.Helper()
	ctx := context.Background()
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/fixture-' || $3, 'publication fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture ' || $3, 'fixture', '{}', 'main', 'awaiting_final_review'
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := db.QueryRowContext(ctx, insertProjectAndRun,
		publicationTestTenantID, publicationTestUserID, marker).Scan(&runID); err != nil {
		t.Fatalf("insert fixture: %v", err)
	}
	return runID
}

// ensurePublication starts a publication row for the fixture run.
func ensurePublication(t *testing.T, queries *sqlc.Queries, runID string) sqlc.RunPublication {
	t.Helper()
	row, err := queries.EnsurePublication(t.Context(), sqlc.EnsurePublicationParams{
		TenantID:        publicationTestTenantID,
		UserID:          publicationTestUserID,
		RunID:           runID,
		Provider:        "noop",
		RemoteBranch:    "agentum/" + runID,
		PublishedCommit: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("ensure publication: %v", err)
	}
	return row
}

// TestEnsurePublicationIsIdempotentPerRun: a repeated ensure for the same run
// creates no second row and changes nothing — "one run, at most one pull
// request" is the unique index's property, and the second caller reads the
// empty return the ON CONFLICT produces.
func TestEnsurePublicationIsIdempotentPerRun(t *testing.T) {
	handle := dbtest.Store(t)
	runID := insertPublicationFixture(t, handle.Store.DB, "ensure-idem")

	first := ensurePublication(t, handle.Queries, runID)
	if first.State != "pending" {
		t.Errorf("first ensure state = %q, want pending", first.State)
	}

	second, err := handle.Queries.EnsurePublication(t.Context(), sqlc.EnsurePublicationParams{
		TenantID:        publicationTestTenantID,
		UserID:          publicationTestUserID,
		RunID:           runID,
		Provider:        "github",
		RemoteBranch:    "agentum/other",
		PublishedCommit: "ffffffffffffffffffffffffffffffffffffffff",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second ensure = (%v, %v); want the empty return the ON CONFLICT produces", second, err)
	}

	readBack, err := handle.Queries.GetPublicationForRun(t.Context(), sqlc.GetPublicationForRunParams{
		TenantID: publicationTestTenantID, RunID: runID,
	})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if readBack.ID != first.ID || readBack.Attempts != 0 {
		t.Errorf("row changed across ensures: id %q (first %q), attempts %d", readBack.ID, first.ID, readBack.Attempts)
	}
}

// TestClaimPublicationExcludesASecondWorkerAndHonoursAnExpiredLease pins the
// lease's two sides: while a live lease is held a second claim reads no row,
// and once the lease has expired the row is claimable again — its worker
// died mid-attempt.
func TestClaimPublicationExcludesASecondWorkerAndHonoursAnExpiredLease(t *testing.T) {
	handle := dbtest.Store(t)
	runID := insertPublicationFixture(t, handle.Store.DB, "claim-lease")
	row := ensurePublication(t, handle.Queries, runID)

	claimed, err := handle.Queries.ClaimPublication(t.Context(), sqlc.ClaimPublicationParams{
		TenantID:       publicationTestTenantID,
		RunID:          runID,
		LeaseOwner:     sql.NullString{String: "worker-a", Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if claimed.State != "publishing" || claimed.Attempts != row.Attempts+1 {
		t.Errorf("claim state = %q, attempts = %d; want publishing, %d", claimed.State, claimed.Attempts, row.Attempts+1)
	}

	if _, err := handle.Queries.ClaimPublication(t.Context(), sqlc.ClaimPublicationParams{
		TenantID:       publicationTestTenantID,
		RunID:          runID,
		LeaseOwner:     sql.NullString{String: "worker-b", Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("claim under a live lease = (%v); want sql.ErrNoRows — the second worker must learn it lost", err)
	}

	if _, err := handle.Store.DB.ExecContext(t.Context(),
		`UPDATE run_publications SET lease_expires_at = now() - interval '1 second' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, err := handle.Queries.ClaimPublication(t.Context(), sqlc.ClaimPublicationParams{
		TenantID:       publicationTestTenantID,
		RunID:          runID,
		LeaseOwner:     sql.NullString{String: "worker-c", Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
	if reclaimed.LeaseOwner.String != "worker-c" || reclaimed.Attempts != 2 {
		t.Errorf("reclaim owner = %q, attempts = %d; want worker-c, 2", reclaimed.LeaseOwner.String, reclaimed.Attempts)
	}
}

// TestClaimPublicationTakesARowWithNoLease: a publishing row with a NULL
// lease is claimable. The claim predicate must match the recovery probe's —
// without the explicit IS NULL branch, three-valued logic leaves such a row
// unclaimable while the probe keeps re-enqueueing it, and the attempts bound
// never grows because the increment lives inside the claim.
func TestClaimPublicationTakesARowWithNoLease(t *testing.T) {
	handle := dbtest.Store(t)
	runID := insertPublicationFixture(t, handle.Store.DB, "claim-null-lease")
	ensurePublication(t, handle.Queries, runID)
	if _, err := handle.Store.DB.ExecContext(t.Context(),
		`UPDATE run_publications SET state = 'publishing', lease_expires_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("null the lease: %v", err)
	}

	claimed, err := handle.Queries.ClaimPublication(t.Context(), sqlc.ClaimPublicationParams{
		TenantID:       publicationTestTenantID,
		RunID:          runID,
		LeaseOwner:     sql.NullString{String: "worker-d", Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("claim with NULL lease: %v", err)
	}
	if claimed.LeaseOwner.String != "worker-d" || claimed.Attempts != 1 {
		t.Errorf("claim owner = %q, attempts = %d; want worker-d, 1", claimed.LeaseOwner.String, claimed.Attempts)
	}
}

// TestFindStalePublicationsReturnsLostWorkOnly: the recovery probe returns a
// pending row with no live publish job and a publishing row with an expired
// lease, and never returns a failed or blocked row — the system restores
// work it did not finish, it does not retry an outcome it recorded. The
// attempts bound keeps a lost row from being re-enqueued forever.
func TestFindStalePublicationsReturnsLostWorkOnly(t *testing.T) {
	handle := dbtest.Store(t)
	lostPending := insertPublicationFixture(t, handle.Store.DB, "stale-pending")
	ensurePublication(t, handle.Queries, lostPending)

	lostPublishing := insertPublicationFixture(t, handle.Store.DB, "stale-publishing")
	ensurePublication(t, handle.Queries, lostPublishing)
	if _, err := handle.Store.DB.ExecContext(t.Context(), `
		UPDATE run_publications SET state = 'publishing',
		    lease_expires_at = now() - interval '1 minute'
		WHERE run_id = $1`, lostPublishing); err != nil {
		t.Fatalf("make publishing stale: %v", err)
	}

	recordedFailure := insertPublicationFixture(t, handle.Store.DB, "stale-failed")
	ensurePublication(t, handle.Queries, recordedFailure)
	failedClaim := claimPublication(t, handle.Queries, recordedFailure, 0, "worker")
	if _, err := handle.Queries.RecordPublicationFailure(t.Context(), sqlc.RecordPublicationFailureParams{
		TenantID: publicationTestTenantID, RunID: recordedFailure, Attempts: failedClaim.Attempts, LeaseOwner: failedClaim.LeaseOwner,
		State:         "failed",
		LastErrorCode: sql.NullString{String: "credentials_missing", Valid: true},
	}); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	recordedBlocked := insertPublicationFixture(t, handle.Store.DB, "stale-blocked")
	ensurePublication(t, handle.Queries, recordedBlocked)
	blockedClaim := claimPublication(t, handle.Queries, recordedBlocked, 0, "worker")
	if _, err := handle.Queries.RecordPublicationFailure(t.Context(), sqlc.RecordPublicationFailureParams{
		TenantID: publicationTestTenantID, RunID: recordedBlocked, Attempts: blockedClaim.Attempts, LeaseOwner: blockedClaim.LeaseOwner,
		State:         "blocked",
		LastErrorCode: sql.NullString{String: "checks_not_passed", Valid: true},
	}); err != nil {
		t.Fatalf("record blocked: %v", err)
	}

	exhausted := insertPublicationFixture(t, handle.Store.DB, "stale-exhausted")
	ensurePublication(t, handle.Queries, exhausted)
	if _, err := handle.Store.DB.ExecContext(t.Context(),
		`UPDATE run_publications SET attempts = 3 WHERE run_id = $1`, exhausted); err != nil {
		t.Fatalf("exhaust attempts: %v", err)
	}

	stale, err := handle.Queries.FindStalePublications(t.Context(), sqlc.FindStalePublicationsParams{
		TenantID: publicationTestTenantID, Attempts: 3,
	})
	if err != nil {
		t.Fatalf("find stale: %v", err)
	}
	returned := map[string]bool{}
	for _, row := range stale {
		returned[row.RunID] = true
	}
	for _, want := range []string{lostPending, lostPublishing} {
		if !returned[want] {
			t.Errorf("probe missed lost publication %s", want)
		}
	}
	for _, refused := range []string{recordedFailure, recordedBlocked, exhausted} {
		if returned[refused] {
			t.Errorf("probe returned a publication it must not touch: %s", refused)
		}
	}
}

func claimPublication(test *testing.T, queries *sqlc.Queries, runID string, requestID int64, owner string) sqlc.RunPublication {
	test.Helper()
	claimed, err := queries.ClaimPublication(test.Context(), sqlc.ClaimPublicationParams{
		TenantID: publicationTestTenantID, RunID: runID, RequestID: requestID,
		LeaseOwner:     sql.NullString{String: owner, Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		test.Fatal(err)
	}
	return claimed
}

func TestPublicationOutcomeRequiresCurrentAttempt(test *testing.T) {
	handle := dbtest.Store(test)
	runID := insertPublicationFixture(test, handle.Store.DB, "fenced-outcome")
	ensurePublication(test, handle.Queries, runID)
	first := claimPublication(test, handle.Queries, runID, 0, "same-worker")
	if _, err := handle.Store.DB.ExecContext(test.Context(), `UPDATE run_publications SET lease_expires_at = now() - interval '1 second' WHERE tenant_id = $1 AND run_id = $2`, publicationTestTenantID, runID); err != nil {
		test.Fatal(err)
	}
	second := claimPublication(test, handle.Queries, runID, 0, "same-worker")
	for _, completed := range []bool{false, true} {
		if completed {
			if _, err := handle.Queries.RecordPublicationSuccess(test.Context(), sqlc.RecordPublicationSuccessParams{
				TenantID: publicationTestTenantID, RunID: runID, Attempts: second.Attempts, LeaseOwner: second.LeaseOwner,
				PrNumber: sql.NullInt32{Int32: 42, Valid: true}, PrUrl: sql.NullString{String: "https://example.com/pull/42", Valid: true},
			}); err != nil {
				test.Fatal(err)
			}
		}
		if _, err := handle.Queries.RecordPublicationSuccess(test.Context(), sqlc.RecordPublicationSuccessParams{
			TenantID: publicationTestTenantID, RunID: runID, Attempts: first.Attempts, LeaseOwner: first.LeaseOwner,
			PrNumber: sql.NullInt32{Int32: 1, Valid: true},
		}); !errors.Is(err, sql.ErrNoRows) {
			test.Fatalf("stale success: %v", err)
		}
		if _, err := handle.Queries.RecordPublicationFailure(test.Context(), sqlc.RecordPublicationFailureParams{
			TenantID: publicationTestTenantID, RunID: runID, Attempts: first.Attempts, LeaseOwner: first.LeaseOwner, State: "failed",
		}); !errors.Is(err, sql.ErrNoRows) {
			test.Fatalf("stale failure: %v", err)
		}
	}
	row, err := handle.Queries.GetPublicationForRun(test.Context(), sqlc.GetPublicationForRunParams{TenantID: publicationTestTenantID, RunID: runID})
	if err != nil {
		test.Fatal(err)
	}
	if row.State != "published" || row.PrNumber.Int32 != 42 || row.PrUrl.String != "https://example.com/pull/42" {
		test.Fatalf("new outcome changed: %+v", row)
	}
}

func TestRecordedPublicationRequiresNewRequest(test *testing.T) {
	for _, state := range []string{"failed", "blocked", "published"} {
		test.Run(state, func(test *testing.T) {
			handle := dbtest.Store(test)
			runID := insertPublicationFixture(test, handle.Store.DB, "request-"+state)
			ensurePublication(test, handle.Queries, runID)
			if _, err := handle.Store.DB.ExecContext(test.Context(), `UPDATE run_publications SET state = $3 WHERE tenant_id = $1 AND run_id = $2`, publicationTestTenantID, runID, state); err != nil {
				test.Fatal(err)
			}
			old := sqlc.ClaimPublicationParams{TenantID: publicationTestTenantID, RunID: runID, LeaseOwner: sql.NullString{String: "old-worker", Valid: true}}
			if _, err := handle.Queries.ClaimPublication(test.Context(), old); !errors.Is(err, sql.ErrNoRows) {
				test.Fatalf("duplicate retried outcome: %v", err)
			}
			requested, err := handle.Queries.RequestPublication(test.Context(), sqlc.RequestPublicationParams{TenantID: publicationTestTenantID, RunID: runID})
			if err != nil {
				test.Fatal(err)
			}
			if _, err := handle.Queries.ClaimPublication(test.Context(), old); !errors.Is(err, sql.ErrNoRows) {
				test.Fatalf("old job consumed new request: %v", err)
			}
			claimPublication(test, handle.Queries, runID, requested.RequestID, "new-worker")
			if _, err := handle.Queries.RequestPublication(test.Context(), sqlc.RequestPublicationParams{TenantID: publicationTestTenantID, RunID: runID}); !errors.Is(err, sql.ErrNoRows) {
				test.Fatalf("request invalidated live lease: %v", err)
			}
		})
	}
}

func TestRecoveryBoundsFailuresBeforePublicationClaim(test *testing.T) {
	handle := dbtest.Store(test)
	runID := insertPublicationFixture(test, handle.Store.DB, "preclaim-failure")
	ensurePublication(test, handle.Queries, runID)
	for attempt := 0; attempt <= 3; attempt++ {
		stale, err := handle.Queries.FindStalePublications(test.Context(), sqlc.FindStalePublicationsParams{TenantID: publicationTestTenantID, Attempts: 3})
		if err != nil {
			test.Fatal(err)
		}
		found := false
		for _, row := range stale {
			if row.RunID == runID {
				found = true
				if row.Attempts != 0 {
					test.Fatal("fixture acquired a lease")
				}
			}
		}
		if found != (attempt < 3) {
			test.Fatalf("after %d pre-claim failures: found = %v", attempt, found)
		}
		if attempt == 3 {
			break
		}
		job, err := handle.Queries.EnqueueJob(test.Context(), sqlc.EnqueueJobParams{TenantID: publicationTestTenantID, UserID: publicationTestUserID, RunID: runID, Kind: "publish", Payload: []byte("{}")})
		if err != nil {
			test.Fatal(err)
		}
		if err := handle.Queries.FailJob(test.Context(), sqlc.FailJobParams{ID: job.ID, LastError: sql.NullString{String: "GetRun failed", Valid: true}}); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := handle.Queries.RequestPublication(test.Context(), sqlc.RequestPublicationParams{TenantID: publicationTestTenantID, RunID: runID}); err != nil {
		test.Fatal(err)
	}
	stale, err := handle.Queries.FindStalePublications(test.Context(), sqlc.FindStalePublicationsParams{TenantID: publicationTestTenantID, Attempts: 3})
	if err != nil {
		test.Fatal(err)
	}
	if len(stale) != 1 || stale[0].RunID != runID {
		test.Fatal("explicit retry inherited old failed-job budget")
	}
}
