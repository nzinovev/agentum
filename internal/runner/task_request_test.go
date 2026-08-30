package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// requestCapturingAdapter records every Invocation's routing block and then
// plays the scripted result, so a test can assert on what the adapter was
// handed without re-implementing the loop.
type requestCapturingAdapter struct {
	stubExecution
	scripts map[string]agent.ResultJSON
	blocks  []string
}

func (adapter *requestCapturingAdapter) Supported() []caps.Category { return fullSupport }

func (adapter *requestCapturingAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	adapter.blocks = append(adapter.blocks, inv.RoutingBlock)
	eventCh := make(chan agent.Event, 1)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stageOf(inv)]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: errors.New("no script for stage")}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{
			SessionID: "sess-" + stageOf(inv), ResultJSON: scripted,
		}}
	}()
	return eventCh, nil
}

// TestRunner_RoutingBlockCarriesTaskRequest is the defect fix itself: the
// routing block handed to the adapter must carry the task's title and
// description — the chain that was entirely missing before (Block had no
// request field, the template no section, the runner no wiring).
func TestRunner_RoutingBlockCarriesTaskRequest(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{
		ID: "Tr", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
		Title:        "Lower the log level of health endpoints",
		Description:  "Log /healthz and /readyz at Debug. Compare by exact path.",
		Overrides:    json.RawMessage(`{"checks":{"required":["verify"]}}`),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &requestCapturingAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "planned"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	if err := runner.Handle(context.Background(), job("run", "Tr", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if len(adapter.blocks) == 0 {
		t.Fatal("the adapter was never invoked; no routing block to inspect")
	}
	block := adapter.blocks[0]
	if !strings.Contains(block, "Lower the log level of health endpoints") {
		t.Errorf("routing block does not carry the task title; got:\n%s", block)
	}
	if !strings.Contains(block, "Log /healthz and /readyz at Debug. Compare by exact path.") {
		t.Errorf("routing block does not carry the task description; got:\n%s", block)
	}
	// The runner side of the same rule: the raw overrides JSON never appears in
	// the block. The check it requests may legitimately arrive through the
	// resolved ## Project checks section (and does not here — this repo declares
	// no registry), but the request blob itself must not.
	if strings.Contains(block, `"required"`) || strings.Contains(block, `"checks":{`) {
		t.Errorf("routing block leaks the raw overrides; got:\n%s", block)
	}
}
