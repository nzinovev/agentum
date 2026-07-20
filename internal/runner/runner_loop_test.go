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
	t           sqlc.Task
	pr          sqlc.Project
	invocations []sqlc.StageInvocation
	events      []sqlc.Event
	enqueued    []string
}

func newFakeStore(t sqlc.Task, pr sqlc.Project) *fakeStore {
	return &fakeStore{t: t, pr: pr}
}

func (s *fakeStore) GetTask(_ context.Context, _ sqlc.GetTaskParams) (sqlc.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t, nil
}
func (s *fakeStore) GetProject(_ context.Context, _ sqlc.GetProjectParams) (sqlc.Project, error) {
	return s.pr, nil
}
func (s *fakeStore) UpdateTaskState(_ context.Context, arg sqlc.UpdateTaskStateParams) (sqlc.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.State = arg.State
	return s.t, nil
}
func (s *fakeStore) UpdateTaskStage(_ context.Context, arg sqlc.UpdateTaskStageParams) (sqlc.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.State = arg.State
	if arg.CurrentStage.Valid {
		s.t.CurrentStage = arg.CurrentStage
	}
	return s.t, nil
}
func (s *fakeStore) CreateStageInvocation(_ context.Context, arg sqlc.CreateStageInvocationParams) (sqlc.StageInvocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := sqlc.StageInvocation{
		ID: fmt.Sprintf("inv-%d", len(s.invocations)+1), TenantID: arg.TenantID, UserID: arg.UserID,
		TaskID: arg.TaskID, Stage: arg.Stage, Sequence: arg.Sequence, ResumeOf: arg.ResumeOf,
	}
	s.invocations = append(s.invocations, inv)
	return inv, nil
}
func (s *fakeStore) FinishStageInvocation(_ context.Context, arg sqlc.FinishStageInvocationParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.invocations {
		if s.invocations[i].ID == arg.ID {
			s.invocations[i].SessionID = arg.SessionID
			s.invocations[i].StopReason = arg.StopReason
			return nil
		}
	}
	return nil
}
func (s *fakeStore) LatestStageForTask(_ context.Context, _ sqlc.LatestStageForTaskParams) (sqlc.StageInvocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.invocations) == 0 {
		return sqlc.StageInvocation{}, sql.ErrNoRows
	}
	return s.invocations[len(s.invocations)-1], nil
}
func (s *fakeStore) AppendEvent(_ context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sqlc.Event{Type: arg.Type, Payload: arg.Payload})
	return sqlc.Event{}, nil
}

func (s *fakeStore) EnqueueJob(_ context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, arg.Kind)
	return sqlc.Job{Kind: arg.Kind}, nil
}

func (s *fakeStore) taskState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.State
}

// scriptAdapter emits a scripted Result per stage. The map stageID→ResultJSON
// defines what each invocation "produces"; an absent stage yields EventError.
type scriptAdapter struct {
	scripts map[string]agent.ResultJSON
}

func (a *scriptAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 2)
	go func() {
		defer close(ch)
		rj, ok := a.scripts[stageOf(inv)]
		if !ok {
			ch <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stageOf(inv))}
			return
		}
		ch <- agent.Event{Kind: agent.EventStream, Chunk: "working..."}
		ch <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stageOf(inv), ResultJSON: rj}}
	}()
	return ch, nil
}

// stageOf recovers the stage id from the routing block the runner rendered.
func stageOf(inv agent.Invocation) string {
	for _, line := range splitLines(inv.RoutingBlock) {
		if mid := substringAfter(line, "stage **"); mid != "" {
			return substringBefore(mid, "**")
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
	pk := scriptPack("spec", map[string]pack.Stage{
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

	src := &staticSource{pk: pk}
	r := New(Deps{Store: store, Packs: src, Adapter: adapter, AgentName: "opencode"})

	// run: spec completes but the human_approval gate pauses.
	if err := r.Handle(t.Context(), job("run", "T1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("after spec run, state = %q, want paused_gate", got)
	}
	if n := len(store.invocations); n != 1 {
		t.Fatalf("expected 1 invocation after run, got %d", n)
	}

	// advance: gate passed → impl auto-advances → done terminal → final gate.
	if err := r.Handle(t.Context(), job("advance", "T1", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	if got := store.taskState(); got != "awaiting_memory_commit" {
		t.Fatalf("after advance, state = %q, want awaiting_memory_commit", got)
	}
	// spec + impl invoked; done is a terminal marker (no invocation).
	if n := len(store.invocations); n != 2 {
		t.Fatalf("expected 2 invocations (spec+impl), got %d", n)
	}
	// The task recorded it reached the final stage.
	store.mu.Lock()
	cur := store.t.CurrentStage.String
	store.mu.Unlock()
	if cur != "done" {
		t.Fatalf("current_stage = %q, want done", cur)
	}
}

func TestRunner_BlockedPausesForOpenQuestions(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	pk := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which framework?"}},
	}}
	r := New(Deps{Store: store, Packs: &staticSource{pk: pk}, Adapter: adapter, AgentName: "opencode"})

	if err := r.Handle(t.Context(), job("run", "T2", "tn", "us")); err != nil {
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
	pk := scriptPack("spec", map[string]pack.Stage{
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
	r := New(Deps{Store: store, Packs: &staticSource{pk: pk}, Adapter: adapter, AgentName: "opencode"})

	done := make(chan error, 1)
	go func() { done <- r.Handle(t.Context(), job("run", "T3", "tn", "us")) }()

	// Wait for the registry to register the run, then cancel it.
	waitForRegistered(r.Cancels(), "T3", 2*time.Second)
	if !r.Cancels().Cancel("T3") {
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

func (s *staticSource) Resolve(_ context.Context, _ string) (*pack.Pack, error) {
	return s.pk, nil
}

// slowAdapter emits its result only after a release signal; used to test cancel.
type slowAdapter struct {
	scripts map[string]agent.ResultJSON
}

func (a *slowAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 2)
	go func() {
		defer close(ch)
		// Block until ctx is cancelled (the cancel handler aborts the run).
		<-ctx.Done()
		ch <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("cancelled: %w", ctx.Err())}
	}()
	return ch, nil
}

func waitForRegistered(reg *CancelRegistry, taskID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reg.mu.Lock()
		_, ok := reg.m[taskID]
		reg.mu.Unlock()
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
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
func substringAfter(s, sep string) string {
	i := indexOf(s, sep)
	if i < 0 {
		return ""
	}
	return s[i+len(sep):]
}
func substringBefore(s, sep string) string {
	i := indexOf(s, sep)
	if i < 0 {
		return s
	}
	return s[:i]
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// keep json import if used later (payload marshaling in fake events)
var _ = json.Marshal
