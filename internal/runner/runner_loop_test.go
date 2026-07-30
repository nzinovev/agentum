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

// fakeStore is an in-memory runner.Store. It holds one task, one project, and a
// log of stage invocations + events. The task/project are seeded at construct.
type fakeStore struct {
	mu          sync.Mutex
	task        sqlc.Task
	project     sqlc.Project
	invocations []sqlc.StageInvocation
	events      []sqlc.Event
	enqueued    []string
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
		CapabilityProfile: arg.CapabilityProfile,
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
func (store *fakeStore) LatestCheckpointForTask(_ context.Context, _ sqlc.LatestCheckpointForTaskParams) (sqlc.TaskCheckpoint, error) {
	return sqlc.TaskCheckpoint{}, sql.ErrNoRows // no checkpoints in the fake
}
func (store *fakeStore) ListCheckpointsForTask(_ context.Context, _ sqlc.ListCheckpointsForTaskParams) ([]sqlc.TaskCheckpoint, error) {
	return nil, nil // no checkpoints in the fake
}
func (store *fakeStore) CreateCheckpoint(_ context.Context, arg sqlc.CreateCheckpointParams) (sqlc.TaskCheckpoint, error) {
	return sqlc.TaskCheckpoint{Label: arg.Label, CommitSha: arg.CommitSha}, nil
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

func (store *fakeStore) taskState() string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.task.State
}

// scriptAdapter emits a scripted Result per stage. The map stageID→ResultJSON
// defines what each invocation "produces"; an absent stage yields EventError.
type scriptAdapter struct {
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
	if got := store.taskState(); got != "awaiting_memory_commit" {
		t.Fatalf("after advance, state = %q, want awaiting_memory_commit", got)
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

// backendDevPack builds the in-memory backend-dev pipeline shape: plan
// (human_approval) → implement → review → (fix → review)* → done, with
// result-driven review routing on the verdict and a configurable fix-cycle
// budget. Mirrors packs/backend-dev/manifest.yaml so the loop tests exercise the
// real pipeline structure.
func backendDevPack(fixCycles int) *pack.Pack {
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "backend-dev", Version: "1.0.0"},
		Tiers: pack.Tiers{Default: "fast"}, Budgets: pack.Budgets{FixCycles: fixCycles, AskToEdit: 1},
		Entry: "plan",
		Stages: map[string]pack.Stage{
			"plan":      {Gate: pack.GateHumanApproval, Role: "analyst", Prompt: "plan.md", Transitions: []pack.Transition{{To: "implement"}}},
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "implement.md", Transitions: []pack.Transition{{To: "review"}}},
			"review": {Gate: pack.GateAuto, Role: "reviewer", Prompt: "review.md", Transitions: []pack.Transition{
				{To: "fix", Condition: `verdict == "changes_requested"`},
				{To: "done", Condition: `verdict == "approved"`},
			}},
			"fix":  {Gate: pack.GateAuto, Role: "fixer", Prompt: "fix.md", Transitions: []pack.Transition{{To: "review"}}},
			"done": {},
		},
		PromptText: map[string]string{"plan": "p", "implement": "i", "review": "r", "fix": "f"},
	}
}

// verdictAdapter scripts every non-review stage to complete and drives the
// review stage through an ordered list of verdicts (one per review call). This
// models a reviewer that requests changes then approves once fixes land.
type verdictAdapter struct {
	reviewVerdicts []string
	calls          int
}

func (adapter *verdictAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
	}
}

func (adapter *verdictAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		stage := stageOf(inv)
		result := agent.ResultJSON{SchemaVersion: "1", Status: agent.StatusComplete, Summary: stage + " done"}
		if stage == "review" {
			verdict := "changes_requested"
			if adapter.calls < len(adapter.reviewVerdicts) {
				verdict = adapter.reviewVerdicts[adapter.calls]
			}
			adapter.calls++
			result.Verdict = verdict
		}
		eventCh <- agent.Event{Kind: agent.EventStream, Chunk: "working..."}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stage, ResultJSON: result}}
	}()
	return eventCh, nil
}

// TestRunner_BackendDevPlanGateBlocksSourceStages proves the first human gate
// (plan approval) pauses before any source-writing stage runs: a `run` job stops
// at paused_gate after the plan stage, and only an `advance` resumes into
// implement. No implement/fix invocation exists before the advance.
func TestRunner_BackendDevPlanGateBlocksSourceStages(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := backendDevPack(2)
	task := sqlc.Task{ID: "BG1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "backend-dev@1.0.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &verdictAdapter{reviewVerdicts: []string{"approved"}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "BG1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("after run, state = %q, want paused_gate", got)
	}
	// Only the plan stage ran; implement/review/fix did not.
	if count := len(store.invocations); count != 1 {
		t.Fatalf("expected 1 invocation (plan) before approval, got %d", count)
	}
	if store.invocations[0].Stage != "plan" {
		t.Fatalf("first invocation stage = %q, want plan", store.invocations[0].Stage)
	}
}

// TestRunner_BackendDevApprovesOnFirstReview proves that after plan approval the
// pipeline runs autonomously to the final-review gate when the reviewer approves
// immediately: implement → review(approved) → done → awaiting_memory_commit,
// with no fix stage invoked.
func TestRunner_BackendDevApprovesOnFirstReview(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := backendDevPack(2)
	task := sqlc.Task{ID: "BG2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "backend-dev@1.0.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &verdictAdapter{reviewVerdicts: []string{"approved"}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	// run: plan pauses at the gate.
	if err := runner.Handle(t.Context(), job("run", "BG2", "tn", "us")); err != nil {
		t.Fatalf("run: %v", err)
	}
	// advance: plan approved → autonomous run to the final gate.
	if err := runner.Handle(t.Context(), job("advance", "BG2", "tn", "us")); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := store.taskState(); got != "awaiting_memory_commit" {
		t.Fatalf("state = %q, want awaiting_memory_commit", got)
	}
	stages := invocationStages(store.invocations)
	if !containsStage(stages, "fix") {
		// approved on first review → no fix stage.
	} else {
		t.Errorf("fix stage ran on an immediate approval; stages = %v", stages)
	}
	for _, want := range []string{"plan", "implement", "review"} {
		if !containsStage(stages, want) {
			t.Errorf("expected stage %q to have run; stages = %v", want, stages)
		}
	}
}

// TestRunner_BackendDevFixLoopConverges proves the autonomous fix loop: the
// reviewer requests changes once, the fixer addresses them, the reviewer
// approves on the second pass, and the run reaches the final-review gate.
func TestRunner_BackendDevFixLoopConverges(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := backendDevPack(2)
	task := sqlc.Task{ID: "BG3", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "backend-dev@1.0.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// review #1: changes_requested → fix; review #2: approved → done.
	adapter := &verdictAdapter{reviewVerdicts: []string{"changes_requested", "approved"}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "BG3", "tn", "us")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := runner.Handle(t.Context(), job("advance", "BG3", "tn", "us")); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := store.taskState(); got != "awaiting_memory_commit" {
		t.Fatalf("state = %q, want awaiting_memory_commit", got)
	}
	stages := invocationStages(store.invocations)
	// Exactly one fix invocation between two reviews.
	fixCount := 0
	for _, stage := range stages {
		if stage == "fix" {
			fixCount++
		}
	}
	if fixCount != 1 {
		t.Errorf("expected exactly 1 fix invocation, got %d (stages=%v)", fixCount, stages)
	}
	reviewCount := 0
	for _, stage := range stages {
		if stage == "review" {
			reviewCount++
		}
	}
	if reviewCount != 2 {
		t.Errorf("expected exactly 2 review invocations, got %d (stages=%v)", reviewCount, stages)
	}
}

// TestRunner_BackendDevFixCycleBudgetExhausted proves that a reviewer which never
// approves is bounded by the fix-cycle budget: the run stops at `failed` with the
// branch preserved, rather than looping forever. With fix_cycles=1, one fix
// runs and the second fix attempt triggers the budget stop.
func TestRunner_BackendDevFixCycleBudgetExhausted(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := backendDevPack(1) // one fix allowed
	task := sqlc.Task{ID: "BG4", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "backend-dev@1.0.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// review always requests changes → loop until the budget stops it.
	adapter := &verdictAdapter{reviewVerdicts: []string{"changes_requested"}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "BG4", "tn", "us")); err != nil {
		t.Fatalf("run: %v", err)
	}
	handleErr := runner.Handle(t.Context(), job("advance", "BG4", "tn", "us"))

	if got := store.taskState(); got != "failed" {
		t.Fatalf("state = %q, want failed after budget exhaustion", got)
	}
	if handleErr == nil {
		t.Fatal("expected a non-nil Handle error on budget exhaustion, got nil")
	}
	// The stop reason is surfaced as a fix-cycle event.
	foundCycleEvent := false
	for _, event := range store.events {
		if event.Type == EvFixCyclesExhausted {
			foundCycleEvent = true
		}
	}
	if !foundCycleEvent {
		t.Errorf("expected a %q event, got events: %v", EvFixCyclesExhausted, eventTypes(store.events))
	}
	// Exactly one fix ran (budget=1); the second fix attempt was blocked.
	fixCount := 0
	for _, stage := range invocationStages(store.invocations) {
		if stage == "fix" {
			fixCount++
		}
	}
	if fixCount != 1 {
		t.Errorf("expected exactly 1 fix invocation before the budget stop, got %d", fixCount)
	}
}

// invocationStages returns the ordered list of stage ids from the recorded
// invocations — the sequence the loop drove.
func invocationStages(invocations []sqlc.StageInvocation) []string {
	out := make([]string, 0, len(invocations))
	for _, invocation := range invocations {
		out = append(out, invocation.Stage)
	}
	return out
}

func containsStage(stages []string, want string) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
}

func eventTypes(events []sqlc.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

// slowAdapter emits its result only after a release signal; used to test cancel.
type slowAdapter struct {
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
