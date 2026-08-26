package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakeStore is an in-memory runner.Store. It holds one task, one project, a
// log of stage invocations + events, and the checkpoints CreateCheckpoint
// recorded. The task/project are seeded at construct.
type fakeStore struct {
	mu          sync.Mutex
	task        sqlc.Task
	project     sqlc.Project
	invocations []sqlc.StageInvocation
	events      []sqlc.Event
	enqueued    []string
	checkpoints []sqlc.TaskCheckpoint
	// approvals maps (taskID, name) -> decision row. ADR 0003 tests seed this to
	// grant or withhold the source_write approval.
	approvals map[string]sqlc.TaskApproval
	// artifactRevisions maps revision name -> current revision. Used by the
	// plan_revision_drift check.
	artifactRevisions map[string]sqlc.ArtifactRevision
}

func newFakeStore(task sqlc.Task, project sqlc.Project) *fakeStore {
	return &fakeStore{task: task, project: project}
}

func (store *fakeStore) GetTask(_ context.Context, _ sqlc.GetTaskParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.task, nil
}
func (store *fakeStore) GetProject(_ context.Context, _ sqlc.GetProjectParams) (sqlc.Project, error) {
	return store.project, nil
}
func (store *fakeStore) UpdateTaskState(_ context.Context, arg sqlc.UpdateTaskStateParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.task.State = arg.State
	return store.task, nil
}
func (store *fakeStore) UpdateTaskStage(_ context.Context, arg sqlc.UpdateTaskStageParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.task.State = arg.State
	if arg.CurrentStage.Valid {
		store.task.CurrentStage = arg.CurrentStage
	}
	return store.task, nil
}
func (store *fakeStore) SetBaseCommit(_ context.Context, arg sqlc.SetBaseCommitParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.task.BaseCommit = arg.BaseCommit
	return store.task, nil
}
func (store *fakeStore) SetResultCommit(_ context.Context, arg sqlc.SetResultCommitParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.task.ResultCommit = arg.ResultCommit
	return store.task, nil
}
func (store *fakeStore) CreateStageInvocation(_ context.Context, arg sqlc.CreateStageInvocationParams) (sqlc.StageInvocation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	invocation := sqlc.StageInvocation{
		ID: fmt.Sprintf("inv-%d", len(store.invocations)+1), TenantID: arg.TenantID, UserID: arg.UserID,
		TaskID: arg.TaskID, Stage: arg.Stage, Sequence: arg.Sequence, ResumeOf: arg.ResumeOf,
		CapabilityProfile: arg.CapabilityProfile, Cycle: arg.Cycle,
	}
	store.invocations = append(store.invocations, invocation)
	return invocation, nil
}
func (store *fakeStore) FinishStageInvocation(_ context.Context, arg sqlc.FinishStageInvocationParams) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for invocationIdx := range store.invocations {
		if store.invocations[invocationIdx].ID == arg.ID {
			store.invocations[invocationIdx].SessionID = arg.SessionID
			store.invocations[invocationIdx].StopReason = arg.StopReason
			return nil
		}
	}
	return nil
}
func (store *fakeStore) LatestStageForTask(_ context.Context, _ sqlc.LatestStageForTaskParams) (sqlc.StageInvocation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.invocations) == 0 {
		return sqlc.StageInvocation{}, sql.ErrNoRows
	}
	return store.invocations[len(store.invocations)-1], nil
}
func (store *fakeStore) MaxCycleForStages(_ context.Context, arg sqlc.MaxCycleForStagesParams) (int32, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var max int32 = -1
	for _, invocation := range store.invocations {
		if invocation.TaskID != arg.TaskID {
			continue
		}
		for _, stageID := range arg.Column3 {
			if invocation.Stage == stageID && invocation.Cycle > max {
				max = invocation.Cycle
			}
		}
	}
	return max, nil
}
func (store *fakeStore) ListStageInvocationsForTask(_ context.Context, arg sqlc.ListStageInvocationsForTaskParams) ([]sqlc.StageInvocation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]sqlc.StageInvocation, 0, len(store.invocations))
	for _, invocation := range store.invocations {
		if invocation.TaskID == arg.TaskID {
			out = append(out, invocation)
		}
	}
	return out, nil
}
func (store *fakeStore) LatestCheckpointForTask(_ context.Context, _ sqlc.LatestCheckpointForTaskParams) (sqlc.TaskCheckpoint, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.checkpoints) == 0 {
		return sqlc.TaskCheckpoint{}, sql.ErrNoRows
	}
	// Return the most recently created checkpoint, mirroring the SQL's
	// ORDER BY created_at DESC LIMIT 1.
	return store.checkpoints[len(store.checkpoints)-1], nil
}
func (store *fakeStore) ListCheckpointsForTask(_ context.Context, _ sqlc.ListCheckpointsForTaskParams) ([]sqlc.TaskCheckpoint, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]sqlc.TaskCheckpoint, len(store.checkpoints))
	copy(out, store.checkpoints)
	return out, nil
}
func (store *fakeStore) CreateCheckpoint(_ context.Context, arg sqlc.CreateCheckpointParams) (sqlc.TaskCheckpoint, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	// Upsert by label, mirroring the SQL's ON CONFLICT (task_id, label).
	for index, checkpoint := range store.checkpoints {
		if checkpoint.Label == arg.Label {
			store.checkpoints[index].CommitSha = arg.CommitSha
			return store.checkpoints[index], nil
		}
	}
	created := sqlc.TaskCheckpoint{Label: arg.Label, CommitSha: arg.CommitSha}
	store.checkpoints = append(store.checkpoints, created)
	return created, nil
}
func (store *fakeStore) AppendEvent(_ context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, sqlc.Event{Type: arg.Type, Payload: arg.Payload})
	return sqlc.Event{}, nil
}

func (store *fakeStore) EnqueueJob(_ context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.enqueued = append(store.enqueued, arg.Kind)
	return sqlc.Job{Kind: arg.Kind}, nil
}

// approvalKey is the map key for fakeStore.approvals: taskID + "/" + name.
func approvalKey(taskID, name string) string { return taskID + "/" + name }

func (store *fakeStore) GetApproval(_ context.Context, arg sqlc.GetApprovalParams) (sqlc.TaskApproval, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if row, ok := store.approvals[approvalKey(arg.TaskID, arg.Name)]; ok {
		return row, nil
	}
	return sqlc.TaskApproval{}, sql.ErrNoRows
}

func (store *fakeStore) ListApprovalsForTask(_ context.Context, arg sqlc.ListApprovalsForTaskParams) ([]sqlc.TaskApproval, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var rows []sqlc.TaskApproval
	for _, row := range store.approvals {
		if row.TaskID == arg.TaskID && row.TenantID == arg.TenantID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (store *fakeStore) CurrentArtifactRevisionForName(_ context.Context, arg sqlc.CurrentArtifactRevisionForNameParams) (sqlc.ArtifactRevision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if rev, ok := store.artifactRevisions[arg.Name]; ok {
		return rev, nil
	}
	return sqlc.ArtifactRevision{}, sql.ErrNoRows
}

func (store *fakeStore) taskState() string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.task.State
}

// scriptAdapter emits a scripted Result per stage. The map stageID→ResultJSON
// defines what each invocation "produces"; an absent stage yields EventError.
type scriptAdapter struct {
	stubExecution
	scripts map[string]agent.ResultJSON
}

// Supported declares every capability category so the runner's profile
// computation is unconstrained by the fake in loop tests (the fake does not
// enforce anything — it only checks loop mechanics).
func (adapter *scriptAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
	}
}

func (adapter *scriptAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stageOf(inv)]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stageOf(inv))}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventStream, Chunk: "working..."}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stageOf(inv), ResultJSON: scripted}}
	}()
	return eventCh, nil
}

// stageOf recovers the stage id from the routing block the runner rendered.
func stageOf(inv agent.Invocation) string {
	for _, line := range splitLines(inv.RoutingBlock) {
		if middle := substringAfter(line, "stage **"); middle != "" {
			return substringBefore(middle, "**")
		}
	}
	return ""
}

// scriptPack builds an in-memory pack: entry → ... → terminal, all auto gates so
// isClean is not exercised (the loop runs against a real temp worktree, and the
// clean check is covered separately by the evaluator tests).
func scriptPack(entry string, stages map[string]pack.Stage) *pack.Pack {
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "test", Version: "0.1.0"},
		Tiers: pack.Tiers{Default: "fast"}, Entry: entry, Stages: stages,
		PromptText: map[string]string{entry: "do the thing"},
	}
}

func TestRunner_RunToPauseThenAdvanceToFinal(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}

	// Pack: spec(human_approval)→impl(auto)→done(terminal).
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {}, // terminal marker
	})

	task := sqlc.Task{ID: "T1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "TestProj"}
	store := newFakeStore(task, proj)

	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done"},
		"impl": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}

	src := &staticSource{pk: taskPack}
	runner := New(Deps{Store: store, Packs: src, Adapter: adapter, AgentName: "opencode"})

	// run: spec completes but the human_approval gate pauses.
	if err := runner.Handle(t.Context(), job("run", "T1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("after spec run, state = %q, want paused_gate", got)
	}
	if count := len(store.invocations); count != 1 {
		t.Fatalf("expected 1 invocation after run, got %d", count)
	}

	// advance: gate passed → impl auto-advances → done terminal → final gate.
	if err := runner.Handle(t.Context(), job("advance", "T1", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("after advance, state = %q, want awaiting_final_review", got)
	}
	// spec + impl invoked; done is a terminal marker (no invocation).
	if count := len(store.invocations); count != 2 {
		t.Fatalf("expected 2 invocations (spec+impl), got %d", count)
	}
	// The task recorded it reached the final stage.
	store.mu.Lock()
	currentStage := store.task.CurrentStage.String
	store.mu.Unlock()
	if currentStage != "done" {
		t.Fatalf("current_stage = %q, want done", currentStage)
	}
}

// TestRunner_PlanApprovalNotApprovedStopsAtPausedGate (ADR 0003 D3/F9): a pack
// declaring a source_write approval, whose task has no approval row, must refuse
// to enter the implementer stage and stop in paused_gate (plan_not_approved)
// pinned to the APPROVAL stage — not the refused stage. The pause point is
// load-bearing: the advance job resolves the current stage's transition, so
// pinning the never-invoked implementer made advance skip it entirely (straight
// to review against an empty diff). Pinned to the plan stage, advance is the
// ordinary plan-gate approval. The plan stage runs with gate:auto here so the
// loop reaches the implementer without a human pause; the refusal is what stops
// it.
func TestRunner_PlanApprovalNotApprovedStopsAtPausedGate(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := approvalGatePack()
	task := sqlc.Task{ID: "Tpa", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// No approvals map seeded → GetApproval returns sql.ErrNoRows → unlock absent.
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"plan":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done"},
		"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "Tpa", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate (plan_not_approved must land in paused_gate so advance/reject are exits, not paused_user_stop whose only exit is continue)", got)
	}
	// The implementer was never invoked — the refusal fired before invokeStage.
	if count := len(store.invocations); count != 1 {
		t.Fatalf("expected 1 invocation (plan only; implementer refused), got %d", count)
	}
	// The pause is pinned to the APPROVAL stage, not the refused implementer.
	// advance resolves the current stage's transition; pinning "implement"
	// (never invoked) there made advance skip the implementer.
	store.mu.Lock()
	currentStage := store.task.CurrentStage.String
	store.mu.Unlock()
	if currentStage != "plan" {
		t.Fatalf("current_stage = %q, want plan (the approval stage — pausing at the refused stage makes advance skip it)", currentStage)
	}
}

// TestRunner_PlanApprovalAdvanceRunsImplementer (F9 regression): after the
// plan_not_approved stop, an advance must run the implementer — not resolve the
// never-run stage's transition and skip to review. The test simulates what the
// advance handler's transaction does (write the approval row bound to the plan
// revision) and then drives the advance job, asserting the run lands at the
// final gate WITH the implementer having actually run.
func TestRunner_PlanApprovalAdvanceRunsImplementer(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := approvalGatePack()
	task := sqlc.Task{ID: "Tpa2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"plan":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done"},
		"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "Tpa2", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("setup: state = %q, want paused_gate before advance", got)
	}

	// What POST .../advance does in its tx: write the approval row bound to the
	// plan artifact's current revision, then enqueue the advance job. The runner
	// test seeds the row and drives the job directly.
	store.mu.Lock()
	store.approvals = map[string]sqlc.TaskApproval{
		approvalKey("Tpa2", "plan"): {TaskID: "Tpa2", TenantID: "tn", Name: "plan", Decision: "approved"},
	}
	store.mu.Unlock()

	if err := runner.Handle(t.Context(), job("advance", "Tpa2", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	// THE anti-regression assertion: the implementer actually ran. With the
	// pin-the-refused-stage bug, advance resolved implement's transition and the
	// run reached the final gate with only the plan invocation.
	if count := len(store.invocations); count != 2 {
		t.Fatalf("after advance, invocations = %d, want 2 (plan + implement; the implementer must run, not be skipped)", count)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("after advance, state = %q, want awaiting_final_review (implement → done terminal → final gate)", got)
	}
	store.mu.Lock()
	currentStage := store.task.CurrentStage.String
	store.mu.Unlock()
	if currentStage != "done" {
		t.Fatalf("current_stage = %q, want done", currentStage)
	}
}

// TestRunner_PlanRevisionDriftAdvanceDoesNotSkip (F9): when the approval is
// granted but the plan was edited after approval, the drift stop must not let
// advance skip the implementer. Advance re-enters, drifts again, and pauses at
// the approval stage once more — a visible no-op retry. Cancel is the exit.
func TestRunner_PlanRevisionDriftAdvanceDoesNotSkip(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := approvalGatePack()
	task := sqlc.Task{ID: "Tdr", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// Approval granted, bound to a plan revision that is no longer current.
	store.approvals = map[string]sqlc.TaskApproval{
		approvalKey("Tdr", "plan"): {TaskID: "Tdr", TenantID: "tn", Name: "plan", Decision: "approved",
			ArtifactRevisionID: nullStr("rev-approved-long-ago")},
	}
	store.artifactRevisions = map[string]sqlc.ArtifactRevision{
		"plan/plan.md": {ID: "rev-edited-after-approval", Name: "plan/plan.md", Kind: "plan_md"},
	}
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"plan":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done"},
		"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "Tdr", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("setup: state = %q, want paused_gate (drift stop)", got)
	}
	if count := len(store.invocations); count != 1 {
		t.Fatalf("setup: invocations = %d, want 1 (implementer refused on drift)", count)
	}

	// Advance (the handler's approval write is a no-op here — the row exists and
	// CreateApproval is ON CONFLICT DO NOTHING — so the runner job alone models
	// the post-request state faithfully).
	if err := runner.Handle(t.Context(), job("advance", "Tdr", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	// Drift persists: paused again at the approval stage, implementer still
	// never invoked. Advance must not resolve the never-run stage's transition.
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("after advance on drift, state = %q, want paused_gate (drift re-fires; advance is a visible no-op retry)", got)
	}
	if count := len(store.invocations); count != 1 {
		t.Fatalf("after advance on drift, invocations = %d, want 1 (the implementer must not be skipped past)", count)
	}
	store.mu.Lock()
	currentStage := store.task.CurrentStage.String
	store.mu.Unlock()
	if currentStage != "plan" {
		t.Fatalf("after advance on drift, current_stage = %q, want plan", currentStage)
	}
}

// approvalGatePack is the F9 fixture: a plan stage with gate:auto (so the loop
// reaches the implementer without a human pause — isolating the refusal) and a
// declared source_write approval the test seeds or leaves absent.
func approvalGatePack() *pack.Pack {
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "test", Version: "0.1.0"},
		Tiers: pack.Tiers{Default: "fast"}, Entry: "plan",
		Approvals: []pack.Approval{{Name: "plan", Stage: "plan", Artifact: "plan.md", Unlocks: "source_write"}},
		Stages: map[string]pack.Stage{
			"plan":      {Gate: pack.GateAuto, Role: "analyst", Prompt: "plan.md", Transitions: []pack.Transition{{To: "implement"}}},
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "implement.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
		PromptText: map[string]string{"plan": "plan", "implement": "implement"},
	}
}

func TestRunner_BlockedPausesForOpenQuestions(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which framework?"}},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "T2", "tn", "us")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := store.taskState(); got != "paused_open_questions" {
		t.Fatalf("state = %q, want paused_open_questions", got)
	}
}

func TestRunner_CancelAbortsInFlightRun(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T3", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	// slowAdapter blocks until the run is cancelled, proving the registry aborts it.
	adapter := &slowAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	done := make(chan error, 1)
	go func() { done <- runner.Handle(t.Context(), job("run", "T3", "tn", "us")) }()

	// Wait for the registry to register the run, then cancel it.
	waitForRegistered(runner.Cancels(), "T3", 2*time.Second)
	if !runner.Cancels().Cancel("T3") {
		t.Fatal("Cancel returned false; expected an in-flight run")
	}
	select {
	case err := <-done:
		// The cancelled run should surface as a non-nil error (ctx cancelled).
		if err == nil {
			t.Log("run returned nil after cancel (acceptable if the loop exited cleanly)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// --- helpers ---

func job(kind, taskID, tenant, user string) sqlc.Job {
	return sqlc.Job{Kind: kind, TaskID: taskID, TenantID: tenant, UserID: user}
}

// staticSource serves a single fixed pack for any ref.
type staticSource struct{ pk *pack.Pack }

func (src *staticSource) Resolve(_ context.Context, _ string) (*pack.Pack, error) {
	return src.pk, nil
}

// slowAdapter emits its result only after a release signal; used to test cancel.
type slowAdapter struct {
	stubExecution
	scripts map[string]agent.ResultJSON
}

// Supported mirrors scriptAdapter.Supported — the fake does not enforce.
func (adapter *slowAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
	}
}

func (adapter *slowAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		// Block until ctx is cancelled (the cancel handler aborts the run).
		<-ctx.Done()
		eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("cancelled: %w", ctx.Err())}
	}()
	return eventCh, nil
}

func waitForRegistered(registry *CancelRegistry, taskID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		_, ok := registry.byTask[taskID]
		registry.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// initRepoWithCommit mirrors the worktree test helper so this test doesn't shell
// out through the real opencode adapter.
func initRepoWithCommit(dir string) error {
	for _, args := range [][]string{
		{"init", "--quiet"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w (%s)", args[0], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"add", "README"}, {"commit", "--quiet", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w (%s)", args[0], err, out)
		}
	}
	return nil
}

// tiny string helpers for parsing the stage id out of the rendered routing block.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index])
			start = index + 1
		}
	}
	return append(lines, text[start:])
}
func substringAfter(text, separator string) string {
	index := indexOf(text, separator)
	if index < 0 {
		return ""
	}
	return text[index+len(separator):]
}
func substringBefore(text, separator string) string {
	index := indexOf(text, separator)
	if index < 0 {
		return text
	}
	return text[:index]
}
func indexOf(text, substr string) int {
	for index := 0; index+len(substr) <= len(text); index++ {
		if text[index:index+len(substr)] == substr {
			return index
		}
	}
	return -1
}

// keep json import if used later (payload marshaling in fake events)
var _ = json.Marshal
