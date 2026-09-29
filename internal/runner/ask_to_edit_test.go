package runner

import (
	"context"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The plan-gate Request-changes path (stage 3 of the run-recovery plan, the
// ask-to-edit slice of the approvals model): remarks accepted at the plan
// gate re-run the planner's session with the text delivered through the
// routing block's Task section, the run pauses at the gate again, and the
// revised plan needs its own approval — the previous decision, had one
// existed, cannot bless it.

// askToEditFixture wires the approval-gate pack over a temp repo and drives
// it to the plan gate.
func newAskToEditFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runID := "T-ate"
	record := sqlc.Run{ID: runID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"plan":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done"},
		"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: approvalGatePack()}, Adapter: adapter})
	fixture := &recoveryFixture{repo: repo, store: store, runner: runner, runID: runID}
	if err := runner.HandleRun(context.Background(), job("run", runID, "tn", "us")); err != nil {
		t.Fatalf("drive run: %v", err)
	}
	// plan runs under gate:auto, then the source_write refusal stops the run
	// at the approval stage in paused_gate (plan_not_approved).
	if state := store.taskState(); state != "paused_gate" {
		t.Fatalf("run state = %q, want paused_gate", state)
	}
	return fixture
}

// remarksJob builds the ask_to_edit job the endpoint enqueues after the
// transition to running.
func remarksJob(runID, text string) sqlc.Job {
	return sqlc.Job{ID: 21, TenantID: "tn", UserID: "us", RunID: runID, Kind: "ask_to_edit",
		Payload: []byte(`{"text":` + quoteJSON(text) + `}`)}
}

func quoteJSON(text string) string {
	// The tests use plain ASCII remarks; a minimal quoting that keeps the
	// payload a valid JSON string.
	escaped := make([]byte, 0, len(text)+2)
	escaped = append(escaped, '"')
	for _, symbol := range []byte(text) {
		switch symbol {
		case '"', '\\':
			escaped = append(escaped, '\\', symbol)
		default:
			escaped = append(escaped, symbol)
		}
	}
	return string(append(escaped, '"'))
}

// TestRunner_AskToEditRerunsPlannerWithRemarks pins the delivery: the remarks
// re-run the PLANNER (a second plan invocation, resuming the captured
// session), the run pauses at the gate again, and no approval exists.
func TestRunner_AskToEditRerunsPlannerWithRemarks(t *testing.T) {
	t.Parallel()
	fixture := newAskToEditFixture(t)
	invocationsBefore := len(fixture.store.invocations)

	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleAskToEdit(context.Background(), remarksJob(fixture.runID, "Cover the retry path in the plan")); err != nil {
		t.Fatalf("ask-to-edit: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate (the revised plan needs its own approval)", state)
	}
	if count := len(fixture.store.invocations); count != invocationsBefore+1 {
		t.Fatalf("invocations = %d, want one more planner attempt", count)
	}
	latest := fixture.store.invocations[len(fixture.store.invocations)-1]
	if latest.Stage != "plan" {
		t.Fatalf("the re-run stage = %q, want plan (the approval stage)", latest.Stage)
	}
	if !latest.ResumeOf.Valid {
		t.Fatal("the planner re-run must resume the captured session")
	}
	if _, approvalErr := fixture.store.GetApproval(context.Background(), sqlc.GetApprovalParams{
		RunID: fixture.runID, TenantID: "tn", Name: "plan",
	}); approvalErr == nil {
		t.Fatal("request changes must not leave an approval behind")
	}
}

// TestRunner_AskToEditThenAdvanceGrantsFreshApproval closes the loop: after
// the revision, an ordinary advance records the plan approval — the gate the
// remarks reopened is passable again, and the implementer runs.
func TestRunner_AskToEditThenAdvanceGrantsFreshApproval(t *testing.T) {
	t.Parallel()
	fixture := newAskToEditFixture(t)
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleAskToEdit(context.Background(), remarksJob(fixture.runID, "Add rollback notes")); err != nil {
		t.Fatalf("ask-to-edit: %v", err)
	}

	// The advance path resolves the plan stage's transition with the
	// approval row written first — mirror what the handler does by seeding
	// the approval the advance handler would have recorded.
	fixture.store.approvals = map[string]sqlc.RunApproval{
		approvalKey(fixture.runID, "plan"): {TenantID: "tn", RunID: fixture.runID, Name: "plan", Decision: "approved"},
	}
	// drive() reads the approval durably at its start, so seeding the row
	// before the job is exactly what a real advance handler produces.
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleAdvance(context.Background(), job("advance", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("advance after revision: %v", err)
	}
	// The implementer ran and the run reached the final gate.
	if state := fixture.store.taskState(); state != "awaiting_final_review" {
		t.Fatalf("state = %q, want awaiting_final_review", state)
	}
	stages := map[string]int{}
	for _, invocation := range fixture.store.invocations {
		stages[invocation.Stage]++
	}
	if stages["implement"] != 1 {
		t.Fatalf("implement invocations = %d, want 1", stages["implement"])
	}
}

// TestRunner_AskToEditTextReachesRoutingBlock pins the channel: the remarks
// appear inside the resumed invocation's routing block (the Task section),
// like a continue's answer — and only there.
func TestRunner_AskToEditTextReachesRoutingBlock(t *testing.T) {
	t.Parallel()
	fixture := newAskToEditFixture(t)
	// Swap the adapter for one that captures the routing block of the second
	// plan invocation.
	captured := &routingCaptureAdapter{scriptAdapter: scriptAdapter{scripts: map[string]agent.ResultJSON{
		"plan":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done"},
		"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}}
	fixture.runner.adapter = captured

	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleAskToEdit(context.Background(), remarksJob(fixture.runID, "Address the migration rollback")); err != nil {
		t.Fatalf("ask-to-edit: %v", err)
	}
	if len(captured.blocks) == 0 {
		t.Fatal("no invocation captured")
	}
	last := captured.blocks[len(captured.blocks)-1]
	if !contains(last, "Address the migration rollback") {
		t.Fatalf("remarks not delivered in the routing block:\n%s", last)
	}
}

// routingCaptureAdapter records each invocation's routing block.
type routingCaptureAdapter struct {
	scriptAdapter
	blocks []string
}

// Invoke records the routing block, then behaves as the scripted adapter.
func (adapter *routingCaptureAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	adapter.blocks = append(adapter.blocks, inv.RoutingBlock)
	return adapter.scriptAdapter.Invoke(ctx, inv)
}
