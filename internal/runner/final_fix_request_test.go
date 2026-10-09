package runner

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestFinalFixRequestReentersFixReviewAndFinalGate: a human request enters
// fixer directly, then review and the terminal checks reopen final review.
func TestFinalFixRequestReentersFixReviewAndFinalGate(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{
		ID: "T-human-fix", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CurrentStage: sql.NullString{String: "fix", Valid: true},
		PreviousResultCommit: sql.NullString{String: "previous-result", Valid: true},
	}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{approvedVerdict("human fix accepted")},
	}
	artifactStore := newRecordingStore()
	if _, err := artifactStore.Put(t.Context(), artifacts.PutParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Name: "final/fix-request.md", Kind: "human_fix_request", Bytes: []byte("Repair the retry path"),
		Actor: artifacts.ActorHuman,
	}); err != nil {
		t.Fatal(err)
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: branchPack(0)}, Adapter: adapter, Artifacts: artifactStore})
	if err := runner.HandleFixRequest(t.Context(), job("fix_request", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if state := store.taskState(); state != "awaiting_final_review" {
		t.Fatalf("state = %q", state)
	}
	if !equalStringSlices(adapter.calls, []string{"fix", "review"}) {
		t.Fatalf("calls = %v", adapter.calls)
	}
	if !strings.Contains(adapter.invocations[0].RoutingBlock, "final/fix-request.md") ||
		!strings.Contains(adapter.invocations[0].RoutingBlock, "Revision: rev-final/fix-request.md") {
		t.Fatalf("fixer routing block lacks the human artifact: %s", adapter.invocations[0].RoutingBlock)
	}
	if store.record.PreviousResultCommit.String != "previous-result" || !store.record.ResultCommit.Valid {
		t.Fatalf("result commits = old %q, new %q", store.record.PreviousResultCommit.String, store.record.ResultCommit.String)
	}
}
