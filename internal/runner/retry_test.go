package runner

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestRetryReentersFailedStageWithFreshSession: a retried run picks up at the
// stage it failed in, not at the pack entry, and does not resume the failed
// attempt's session — that session ended in the error being retried past.
// The run's history keeps the cause through the run.retried event, because
// the transition back to running cleared runs.error.
func TestRetryReentersFailedStageWithFreshSession(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	// The retry endpoint has already moved the run failed → running.
	record := sqlc.Run{ID: "T-retry", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0",
		CurrentStage: sql.NullString{String: "impl", Valid: true}}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"})
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		RunID: record.ID, TenantID: record.TenantID, Stage: "impl",
		SessionID: sql.NullString{String: "failed-impl-session", Valid: true},
	})
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &sequenceAdapter{results: []agent.ResultJSON{{SchemaVersion: "1", Status: agent.StatusComplete}}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})

	retryJob := job(jobKindRetry, record.ID, record.TenantID, record.UserID)
	retryJob.Payload = []byte(`{"previous_error":"impl: adapter crashed"}`)
	if err := runner.HandleRetry(t.Context(), retryJob); err != nil {
		t.Fatal(err)
	}
	if len(adapter.invocations) != 1 || adapter.invocations[0].ResumeSession != "" {
		t.Fatalf("invocations = %+v, want one fresh invocation", adapter.invocations)
	}
	if entered := store.invocations[len(store.invocations)-1].Stage; entered != "impl" {
		t.Errorf("retry entered stage %q, want the failed stage impl", entered)
	}
	if store.record.State != "awaiting_final_review" {
		t.Errorf("state after retry = %s, want awaiting_final_review", store.record.State)
	}
	requireRecoveryEvent(t, store, EvRunRetried)
	for _, event := range store.events {
		if event.Type != EvRunRetried {
			continue
		}
		var payload struct {
			PreviousError string `json:"previous_error"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.PreviousError != "impl: adapter crashed" {
			t.Errorf("run.retried payload = %s, want the previous error", event.Payload)
		}
	}
}
