package runner

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type pauseDuringInvocationAdapter struct {
	sequenceAdapter
	store     *fakeStore
	requested bool
}

func (adapter *pauseDuringInvocationAdapter) Invoke(ctx context.Context, invocation agent.Invocation) (<-chan agent.Event, error) {
	if !adapter.requested {
		adapter.store.mu.Lock()
		adapter.store.record.PauseRequestedAt = sql.NullTime{Time: time.Now(), Valid: true}
		adapter.store.mu.Unlock()
		adapter.requested = true
	}
	return adapter.sequenceAdapter.Invoke(ctx, invocation)
}

// TestUserPauseStopsAfterCheckpoint: the current invocation finishes, its
// checkpoint persists, and Continue delivers a note to the next invocation.
func TestUserPauseStopsAfterCheckpoint(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "T-pause", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	adapter := &pauseDuringInvocationAdapter{store: store}
	adapter.results = []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "first"},
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "second"},
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})
	if err := runner.HandleRun(t.Context(), job("run", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if state := store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q", state)
	}
	if !hasStopReason(store.events, "user_pause") {
		t.Fatal("user_pause event missing")
	}
	if len(store.invocations) != 1 {
		t.Fatalf("invocations before Continue = %d", len(store.invocations))
	}
	if len(store.checkpoints) < 2 || store.checkpoints[len(store.checkpoints)-1].Label != "post-spec" {
		t.Fatalf("checkpoint after invocation missing: %+v", store.checkpoints)
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{ID: record.ID, TenantID: record.TenantID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	continuation, err := continueJob(record.ID, record.TenantID, record.UserID, "Please cover the retry path")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.HandleContinue(t.Context(), continuation); err != nil {
		t.Fatal(err)
	}
	if state := store.taskState(); state != "awaiting_final_review" {
		t.Fatalf("state after Continue = %q", state)
	}
	if !strings.Contains(adapter.invocations[1].RoutingBlock, "Please cover the retry path") {
		t.Fatal("note missing from resumed invocation")
	}
}
