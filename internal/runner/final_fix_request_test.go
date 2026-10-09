package runner

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/pack"
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
		PreviousResultCommit:       sql.NullString{String: "previous-result", Valid: true},
		ActiveFixRequestRevisionID: sql.NullString{String: "rev-final/fix-request.md", Valid: true},
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
	if !strings.Contains(adapter.invocations[1].RoutingBlock, "final/fix-request.md") {
		t.Fatalf("reviewer routing block lacks the human artifact: %s", adapter.invocations[1].RoutingBlock)
	}
	if store.record.PreviousResultCommit.String != "previous-result" || !store.record.ResultCommit.Valid {
		t.Fatalf("result commits = old %q, new %q", store.record.PreviousResultCommit.String, store.record.ResultCommit.String)
	}
}

// TestHumanFixRequestDoesNotSpendAutomaticBudget keeps reviewer requested
// corrections available after the pack's automatic fix cycle count is spent.
func TestHumanFixRequestDoesNotSpendAutomaticBudget(t *testing.T) {
	record := sqlc.Run{ID: "T-human-budget", TenantID: "tn", UserID: "us",
		ActiveFixRequestRevisionID: sql.NullString{String: "human-note", Valid: true}}
	store := newFakeStore(record, sqlc.Project{})
	runPack := &pack.Pack{Budgets: pack.Budgets{FixCycles: 0}, Stages: map[string]pack.Stage{
		"review": {Role: "reviewer", Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "fix"}}},
		"fix":    {Role: "fixer", Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "review"}}},
	}}
	runner := New(Deps{Store: store})
	transitionContext, err := runner.buildTransitionContext(t.Context(), record, runPack, "fix", nil)
	if err != nil {
		t.Fatal(err)
	}
	if transitionContext.Budget <= transitionContext.FixCyclesUsed {
		t.Fatalf("human fix budget = %d, used = %d", transitionContext.Budget, transitionContext.FixCyclesUsed)
	}
	transitionContext.Verdict = "changes_requested"
	resolution, err := ResolveTransition(runPack.Stages["review"], "review", transitionContext)
	if err != nil || resolution.To != "fix" || resolution.StopReason != "" {
		t.Fatalf("human fix transition = %+v, err = %v", resolution, err)
	}
}

// TestHumanFixReviewerFeedbackWaitsForDeveloper keeps a reviewed correction
// paused until a new human note starts another fixer invocation.
func TestHumanFixReviewerFeedbackWaitsForDeveloper(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-human-review-feedback", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", CurrentStage: sql.NullString{String: "fix", Valid: true},
		ActiveFixRequestRevisionID: sql.NullString{String: "rev-final/fix-request.md", Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{changesRequested("retry path still fails"), approvedVerdict("fixed")},
	}
	artifactStore := newRecordingStore()
	if _, err := artifactStore.Put(t.Context(), artifacts.PutParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Name: "final/fix-request.md", Kind: "human_fix_request", Bytes: []byte("Repair retry path"), Actor: artifacts.ActorHuman,
	}); err != nil {
		t.Fatal(err)
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: branchPack(0)}, Adapter: adapter, Artifacts: artifactStore})
	if err := runner.HandleFixRequest(t.Context(), job("fix_request", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if store.record.State != "paused_user_stop" || store.record.CurrentStage.String != "fix" ||
		store.record.StopReason != "human_fix_feedback_required" || !equalStringSlices(adapter.calls, []string{"fix", "review"}) {
		t.Fatalf("review feedback stop = %+v, calls = %v", store.record, adapter.calls)
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: record.ID, TenantID: record.TenantID, State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "Address the new reviewer finding")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if store.record.State != "awaiting_final_review" || !equalStringSlices(adapter.calls, []string{"fix", "review", "fix", "review"}) {
		t.Fatalf("retry state = %s, calls = %v", store.record.State, adapter.calls)
	}
	if !strings.Contains(adapter.invocations[2].RoutingBlock, "Address the new reviewer finding") ||
		!strings.Contains(adapter.invocations[2].RoutingBlock, "Reviewer findings to address") ||
		adapter.invocations[2].ResumeSession != "" {
		t.Fatalf("retry fixer routing = %+v", adapter.invocations[2])
	}
}
