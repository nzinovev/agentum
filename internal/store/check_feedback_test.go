package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestCheckFeedbackRoundIsOncePerDeliveryAttempt: the runner gets one
// automatic check-feedback round, and only a person moving the run back to
// running gives it another. A fake would re-implement the conditional UPDATE
// from the same reading this test checks, so it runs against Postgres.
func TestCheckFeedbackRoundIsOncePerDeliveryAttempt(t *testing.T) {
	handle := dbtest.Store(t)
	runID := insertPublicationFixture(t, handle.Store.DB, "check-feedback")
	ctx := context.Background()
	setState := func(state string) {
		t.Helper()
		if _, err := handle.Queries.UpdateRunState(ctx, sqlc.UpdateRunStateParams{
			ID: runID, TenantID: publicationTestTenantID, State: state,
		}); err != nil {
			t.Fatalf("set state %s: %v", state, err)
		}
	}
	claim := func() error {
		_, err := handle.Queries.ClaimCheckFeedbackRound(ctx, sqlc.ClaimCheckFeedbackRoundParams{
			ID: runID, TenantID: publicationTestTenantID,
		})
		return err
	}

	setState("running")
	if err := claim(); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := claim(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second claim in the same attempt = %v, want sql.ErrNoRows", err)
	}
	// The runner's own stage writes keep the run running; they must not
	// refill the round.
	if _, err := handle.Queries.UpdateRunStage(ctx, sqlc.UpdateRunStageParams{
		ID: runID, TenantID: publicationTestTenantID,
		CurrentStage: sql.NullString{String: "implement", Valid: true}, State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	setState("running")
	if err := claim(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim after a running-to-running write = %v, want sql.ErrNoRows", err)
	}

	// A failed run that a person retries is a new delivery attempt.
	setState("failed")
	setState("running")
	if err := claim(); err != nil {
		t.Fatalf("claim after retry: %v", err)
	}
}
