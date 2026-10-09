package runner

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestPauseBeforeNextStageKeepsTarget: an advance interrupted before its first
// invocation continues at the target stage without the planner's session.
func TestPauseBeforeNextStageKeepsTarget(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-pause-advance", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0",
		CurrentStage:     sql.NullString{String: "spec", Valid: true},
		PauseRequestedAt: sql.NullTime{Time: time.Now(), Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		RunID: record.ID, TenantID: record.TenantID, Stage: "spec",
		SessionID: sql.NullString{String: "spec-session", Valid: true},
	})
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &sequenceAdapter{results: []agent.ResultJSON{{SchemaVersion: "1", Status: agent.StatusComplete}}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})
	if err := runner.HandleAdvance(t.Context(), job("advance", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if store.record.State != "paused_user_stop" || store.record.CurrentStage.String != "impl" {
		t.Fatalf("pause position = %s/%s", store.record.State, store.record.CurrentStage.String)
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{ID: record.ID, TenantID: record.TenantID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "Cover retries")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if len(adapter.invocations) != 1 || !strings.Contains(adapter.invocations[0].RoutingBlock, "Cover retries") || adapter.invocations[0].ResumeSession != "" {
		t.Fatalf("next invocation = %+v", adapter.invocations)
	}
	if store.record.State != "awaiting_final_review" {
		t.Fatalf("state after Continue = %s", store.record.State)
	}
}

// TestPauseAtTerminalDeliversContinueText: a stop after checks pins the terminal
// marker, and a note re-enters the previous agent stage before checks rerun.
func TestPauseAtTerminalDeliversContinueText(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-pause-terminal", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0",
		CurrentStage:     sql.NullString{String: "review", Valid: true},
		PauseRequestedAt: sql.NullTime{Time: time.Now(), Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		RunID: record.ID, TenantID: record.TenantID, Stage: "review",
		SessionID: sql.NullString{String: "review-session", Valid: true},
	})
	runPack := scriptPack("review", map[string]pack.Stage{
		"review": {Gate: pack.GateAuto, Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":   {},
	})
	adapter := &sequenceAdapter{results: []agent.ResultJSON{{SchemaVersion: "1", Status: agent.StatusComplete}}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})
	if err := runner.HandleRun(t.Context(), job("run", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if store.record.CurrentStage.String != "review" {
		t.Fatalf("pre-stage pause = %s", store.record.CurrentStage.String)
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{ID: record.ID, TenantID: record.TenantID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	// Simulate the request arriving during checks, after the review checkpoint.
	store.mu.Lock()
	store.record.CurrentStage = sql.NullString{String: "review", Valid: true}
	store.record.PauseRequestedAt = sql.NullTime{Time: time.Now(), Valid: true}
	store.mu.Unlock()
	_, err := runner.processStage(t.Context(), stageRun{record: store.record, runPack: runPack}, "done", "", "", stageTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if store.record.State != "paused_user_stop" || store.record.CurrentStage.String != "done" {
		t.Fatalf("terminal pause position = %s/%s", store.record.State, store.record.CurrentStage.String)
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{ID: record.ID, TenantID: record.TenantID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "Review the timeout")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if len(adapter.invocations) != 1 || adapter.invocations[0].ResumeSession != "" ||
		!strings.Contains(adapter.invocations[0].RoutingBlock, "Review the timeout") {
		t.Fatalf("resumed review = %+v", adapter.invocations)
	}
}

// TestPauseAfterContinueQueuesNoteBeforeStopping ensures a second pause
// request cannot consume a queued human note before its first invocation.
func TestPauseAfterContinueQueuesNoteBeforeStopping(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-pause-after-continue", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0",
		CurrentStage:     sql.NullString{String: "impl", Valid: true},
		PauseRequestedAt: sql.NullTime{Time: time.Now(), Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		RunID: record.ID, TenantID: record.TenantID, Stage: "spec",
		SessionID: sql.NullString{String: "spec-session", Valid: true},
	})
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &sequenceAdapter{results: []agent.ResultJSON{{SchemaVersion: "1", Status: agent.StatusComplete}}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "Check the timeout")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if len(adapter.invocations) != 1 || adapter.invocations[0].ResumeSession != "" ||
		!strings.Contains(adapter.invocations[0].RoutingBlock, "Check the timeout") {
		t.Fatalf("first invocation after Continue = %+v", adapter.invocations)
	}
	if store.record.State != "paused_user_stop" {
		t.Fatalf("state after queued note and pause = %s", store.record.State)
	}
}

// TestPauseBeforeFixerKeepsReviewerFindings: a pause pinned on the fixer loses
// the review → fix edge, so Continue must point the fresh fixer invocation at
// the reviewer's verdict from the durable artifact, with no human fix request
// involved.
func TestPauseBeforeFixerKeepsReviewerFindings(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-pause-before-fix", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0",
		CurrentStage: sql.NullString{String: "fix", Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		RunID: record.ID, TenantID: record.TenantID, Stage: "review",
		SessionID: sql.NullString{String: "review-session", Valid: true},
	})
	verdictBytes, err := json.Marshal(changesRequested("retry path still fails"))
	if err != nil {
		t.Fatal(err)
	}
	artifactStore := newRecordingStore()
	if _, err := artifactStore.Put(t.Context(), artifacts.PutParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Name: "review/" + agent.VerdictFileName, Kind: "verdict_json", Bytes: verdictBytes, Actor: artifacts.ActorAgent,
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{approvedVerdict("fixed")},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: branchPack(1)}, Adapter: adapter, Artifacts: artifactStore})
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if len(adapter.calls) == 0 || adapter.calls[0] != "fix" || adapter.invocations[0].ResumeSession != "" {
		t.Fatalf("first invocation after Continue = %v, resume %q", adapter.calls, adapter.invocations[0].ResumeSession)
	}
	if !strings.Contains(adapter.invocations[0].RoutingBlock, "Reviewer findings to address") ||
		strings.Contains(adapter.invocations[0].RoutingBlock, "Human fix request to address") {
		t.Fatalf("fixer routing block = %s", adapter.invocations[0].RoutingBlock)
	}
}
