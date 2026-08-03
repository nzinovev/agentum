package artifacts

import (
	"errors"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// planRevision is the decision the revision chain turns on, lifted out of the
// transaction so it can be pinned without a database. The transaction wiring
// around it (the row lock, the demote-by-id, the commit) is exercised against a
// real Postgres in the integration layer; what is testable here is the
// precedence between the three outcomes, which is where a lost update or a
// silently-accepted stale write would come from.

func revision(id, hash string) sqlc.ArtifactRevision {
	return sqlc.ArtifactRevision{ID: id, ContentHash: hash, IsCurrent: true}
}

// TestPlanRevision_FirstCreateChainsToNothing: no prior current revision means
// the write is a create with a NULL prev_revision_id.
func TestPlanRevision_FirstCreateChainsToNothing(t *testing.T) {
	t.Parallel()
	plan, err := planRevision(sqlc.ArtifactRevision{}, false, "", "hash-a")
	if err != nil {
		t.Fatalf("planRevision: %v", err)
	}
	if plan.noop {
		t.Error("first create planned as a no-op")
	}
	if plan.action() != ActionCreate {
		t.Errorf("action = %q, want create", plan.action())
	}
	if plan.prev().Valid {
		t.Errorf("prev = %v, want NULL for a create", plan.prev())
	}
}

// TestPlanRevision_EditChainsToTheCurrentRevision: new bytes over an existing
// artifact chain to whatever the transaction found current, not to whatever the
// caller last saw.
func TestPlanRevision_EditChainsToTheCurrentRevision(t *testing.T) {
	t.Parallel()
	plan, err := planRevision(revision("rev-1", "hash-a"), true, "", "hash-b")
	if err != nil {
		t.Fatalf("planRevision: %v", err)
	}
	if plan.action() != ActionEdit {
		t.Errorf("action = %q, want edit", plan.action())
	}
	if !plan.prev().Valid || plan.prev().String != "rev-1" {
		t.Errorf("prev = %+v, want rev-1", plan.prev())
	}
}

// TestPlanRevision_IdenticalContentIsANoop: re-storing the same bytes must not
// grow the chain with a revision that changed nothing.
func TestPlanRevision_IdenticalContentIsANoop(t *testing.T) {
	t.Parallel()
	plan, err := planRevision(revision("rev-1", "hash-a"), true, "", "hash-a")
	if err != nil {
		t.Fatalf("planRevision: %v", err)
	}
	if !plan.noop {
		t.Fatal("identical content planned a new revision")
	}
	if plan.current.ID != "rev-1" {
		t.Errorf("current = %q, want rev-1 (the row to return unchanged)", plan.current.ID)
	}
}

// TestPlanRevision_StalePreconditionConflicts is the lost-update guard. A
// caller that composed an edit against rev-1 must not have it silently applied
// on top of rev-2 that someone else wrote in between.
func TestPlanRevision_StalePreconditionConflicts(t *testing.T) {
	t.Parallel()
	_, err := planRevision(revision("rev-2", "hash-b"), true, "rev-1", "hash-c")
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want ErrRevisionConflict", err)
	}
}

// TestPlanRevision_PreconditionOnAnAbsentArtifactConflicts: pinning a revision
// of an artifact that has none is just as stale — the row was demoted or the
// caller is pointing at another task's revision.
func TestPlanRevision_PreconditionOnAnAbsentArtifactConflicts(t *testing.T) {
	t.Parallel()
	_, err := planRevision(sqlc.ArtifactRevision{}, false, "rev-1", "hash-a")
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want ErrRevisionConflict", err)
	}
}

// TestPlanRevision_MatchingPreconditionProceeds: the precondition is a guard,
// not a second write path — when it holds, the plan is the ordinary edit.
func TestPlanRevision_MatchingPreconditionProceeds(t *testing.T) {
	t.Parallel()
	plan, err := planRevision(revision("rev-1", "hash-a"), true, "rev-1", "hash-b")
	if err != nil {
		t.Fatalf("planRevision: %v", err)
	}
	if plan.action() != ActionEdit || plan.prev().String != "rev-1" {
		t.Errorf("plan = %+v, want an edit chained to rev-1", plan)
	}
}

// TestPlanRevision_StalePreconditionBeatsIdenticalContent pins the precedence
// the two guards have against each other. A caller whose pinned revision is
// gone has lost the race whatever the bytes say; returning the current revision
// as a successful no-op would tell them their edit landed on the revision they
// were looking at, which is exactly the thing the precondition exists to deny.
func TestPlanRevision_StalePreconditionBeatsIdenticalContent(t *testing.T) {
	t.Parallel()
	_, err := planRevision(revision("rev-2", "hash-a"), true, "rev-1", "hash-a")
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want ErrRevisionConflict", err)
	}
}

// TestIsCurrentRevisionConflict recognizes the unique-index violation two
// racing first-creates produce. There is no row for them to lock, so the
// partial index is the only thing serializing them, and the loser's error has
// to reach the caller as a conflict rather than as an opaque 500.
func TestIsCurrentRevisionConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "postgres unique violation",
			err:  errors.New(`pq: duplicate key value violates unique constraint "idx_artifact_rev_current"`),
			want: true,
		},
		{name: "unrelated error", err: errors.New("connection refused"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, table := range cases {
		t.Run(table.name, func(t *testing.T) {
			t.Parallel()
			if got := isCurrentRevisionConflict(table.err); got != table.want {
				t.Errorf("isCurrentRevisionConflict(%v) = %v, want %v", table.err, got, table.want)
			}
		})
	}
}
