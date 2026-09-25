package runner

import (
	"database/sql"
	"errors"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The final gate's publication hook is best-effort by contract: the run's
// transition to the review gate must survive either of its two writes
// failing, and a disabled hook must write nothing at all. These tests pin
// that contract against the fake store, without a git fixture — the
// transition under test needs only a pinned result commit.

// gateRunner builds a runner over the fake store with the publication hook
// configured. The record starts as a running run with its result commit
// already pinned, which is the state transitionToFinalState receives at the
// gate.
func gateRunner(store *fakeStore, hook PublicationHook) *Runner {
	return New(Deps{
		Store:       store,
		Log:         slog.New(slog.DiscardHandler),
		Publication: hook,
	})
}

// gateRecord returns the run row as the gate sees it.
func gateRecord(store *fakeStore) sqlc.Run {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.record
}

// TestFinalGatePublicationHookCreatesRowAndJob: an enabled hook writes the
// pending publication row (remote branch, pinned result commit) and enqueues
// the publish job — the run is now reviewable with a delivery on its way.
func TestFinalGatePublicationHookCreatesRowAndJob(t *testing.T) {
	t.Parallel()
	store := newFakeStore(sqlc.Run{
		ID: "T-pub-1", TenantID: "tn", UserID: "us", State: "running",
		ResultCommit: sql.NullString{String: "abc123", Valid: true},
	}, sqlc.Project{})
	runnerInstance := gateRunner(store, PublicationHook{Enabled: true, Provider: "noop"})

	if err := runnerInstance.transitionToFinalState(t.Context(), store.record, "", "review"); err != nil {
		t.Fatalf("transitionToFinalState: %v", err)
	}
	if state := gateRecord(store).State; state != "awaiting_final_review" {
		t.Errorf("run state = %q, want awaiting_final_review", state)
	}
	if len(store.publications) != 1 {
		t.Fatalf("publication rows = %d, want 1", len(store.publications))
	}
	row := store.publications[0]
	if row.State != "pending" || row.Provider != "noop" {
		t.Errorf("row = %+v; want pending under provider noop", row)
	}
	if row.RemoteBranch != "agentum/T-pub-1" {
		t.Errorf("remote branch = %q, want agentum/T-pub-1", row.RemoteBranch)
	}
	if row.PublishedCommit != "abc123" {
		t.Errorf("published commit = %q, want the pinned result commit", row.PublishedCommit)
	}
	if len(store.enqueued) != 1 || store.enqueued[0] != "publish" {
		t.Errorf("enqueued = %v, want [publish]", store.enqueued)
	}
}

// TestFinalGatePublicationHookDisabledWritesNothing: with publication off,
// reaching the gate creates no row and enqueues no job — a run on a
// repository without a remote never sees a publication error.
func TestFinalGatePublicationHookDisabledWritesNothing(t *testing.T) {
	t.Parallel()
	store := newFakeStore(sqlc.Run{
		ID: "T-pub-off", TenantID: "tn", UserID: "us", State: "running",
		ResultCommit: sql.NullString{String: "abc123", Valid: true},
	}, sqlc.Project{})
	runnerInstance := gateRunner(store, PublicationHook{})

	if err := runnerInstance.transitionToFinalState(t.Context(), store.record, "", "review"); err != nil {
		t.Fatalf("transitionToFinalState: %v", err)
	}
	if state := gateRecord(store).State; state != "awaiting_final_review" {
		t.Errorf("run state = %q, want awaiting_final_review", state)
	}
	if len(store.publications) != 0 || len(store.enqueued) != 0 {
		t.Errorf("disabled hook wrote publications=%d enqueued=%v; want nothing", len(store.publications), store.enqueued)
	}
}

// TestFinalGatePublicationFailuresDoNotBlockTheGate: either of the hook's
// writes failing leaves the run at the review gate with its branch and result
// commit intact — the hook is delivery, and delivery is best-effort.
func TestFinalGatePublicationFailuresDoNotBlockTheGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		wire func(store *fakeStore)
	}{
		{"row write fails", func(store *fakeStore) { store.publicationErr = errors.New("db down") }},
		{"job enqueue fails", func(store *fakeStore) { store.enqueueErr = errors.New("db down") }},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore(sqlc.Run{
				ID: "T-pub-fail", TenantID: "tn", UserID: "us", State: "running",
				ResultCommit: sql.NullString{String: "abc123", Valid: true},
			}, sqlc.Project{})
			testCase.wire(store)
			runnerInstance := gateRunner(store, PublicationHook{Enabled: true, Provider: "noop"})

			if err := runnerInstance.transitionToFinalState(t.Context(), store.record, "", "review"); err != nil {
				t.Fatalf("transitionToFinalState: %v; a publication failure must not fail the gate", err)
			}
			if state := gateRecord(store).State; state != "awaiting_final_review" {
				t.Errorf("run state = %q, want awaiting_final_review", state)
			}
		})
	}
}
