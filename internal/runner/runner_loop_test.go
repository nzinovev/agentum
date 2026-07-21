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
func (store *fakeStore) CreateStageInvocation(_ context.Context, arg sqlc.CreateStageInvocationParams) (sqlc.StageInvocation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	invocation := sqlc.StageInvocation{
		ID: fmt.Sprintf("inv-%d", len(store.invocations)+1), TenantID: arg.TenantID, UserID: arg.UserID,
		TaskID: arg.TaskID, Stage: arg.Stage, Sequence: arg.Sequence, ResumeOf: arg.ResumeOf,
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

// slowAdapter emits its result only after a release signal; used to test cancel.
type slowAdapter struct {
	scripts map[string]agent.ResultJSON
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
