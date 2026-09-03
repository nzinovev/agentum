package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// These tests pin the orchestrator's own gate decisions: a gate that could
// have stopped the work and did not is a decision, recorded under the system
// actor with the run's user id — distinct from a human's decision on the same
// stage, and absent when there was no gate to pass (plain auto) or when the
// gate did stop the work (the human decides then, and their handlers record
// it).

// systemDecisions collects the system-actor gate decisions the fake manifest
// service received.
func systemDecisions(service *fakeManifestService) []manifest.GateDecision {
	service.mu.Lock()
	defer service.mu.Unlock()
	var decisions []manifest.GateDecision
	for _, patch := range service.addEvidence {
		for _, decision := range patch.GateDecisions {
			if decision.Actor == string(authz.ActorSystem) {
				decisions = append(decisions, decision)
			}
		}
	}
	return decisions
}

// dirtyTreeAdapter makes the stage leave an undeclared file behind in the
// worktree, so the auto_if_clean gate sees a dirty tree.
type dirtyTreeAdapter struct {
	scriptAdapter
}

func (adapter *dirtyTreeAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	if err := os.WriteFile(filepath.Join(inv.Workdir, "undeclared.txt"), []byte("stray"), 0o644); err != nil {
		return nil, err
	}
	return adapter.scriptAdapter.Invoke(ctx, inv)
}

func TestRunner_AutoIfCleanPassRecordsSystemDecision(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "T-aic", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAutoIfClean, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	manifestFake := &fakeManifestService{}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, Manifest: nil})
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", "T-aic", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}

	decisions := systemDecisions(manifestFake)
	if len(decisions) != 1 {
		t.Fatalf("auto_if_clean passing must record exactly one system decision; got %d (%+v)", len(decisions), decisions)
	}
	decision := decisions[0]
	if decision.Stage != "spec" || decision.Gate != "auto_if_clean" || decision.Decision != "approved" {
		t.Fatalf("decision = %+v, want {spec auto_if_clean approved}", decision)
	}
	if decision.UserID != "us" {
		t.Fatalf("decision.UserID = %q, want the run's user (whose behalf the orchestrator acted on)", decision.UserID)
	}
}

func TestRunner_PlainAutoGateRecordsNoDecision(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "T-auto", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	manifestFake := &fakeManifestService{}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", "T-auto", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if decisions := systemDecisions(manifestFake); len(decisions) != 0 {
		t.Fatalf("a plain auto stage has no gate and no decision to record; got %+v", decisions)
	}
}

func TestRunner_AutoIfCleanDirtyTreeStopsForHumanWithoutSystemDecision(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "T-dirty", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAutoIfClean, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &dirtyTreeAdapter{scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}}
	manifestFake := &fakeManifestService{}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", "T-dirty", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate — a dirty tree is the gate stopping the work", got)
	}
	// The gate DID stop the work; the human decides it, and the API handlers
	// record that decision. A system "approved" here would be a false record.
	if decisions := systemDecisions(manifestFake); len(decisions) != 0 {
		t.Fatalf("a gate that stopped the work records no system decision; got %+v", decisions)
	}
}

func TestRunner_AutoOnApprovalAdvanceRecordsSystemDecision(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	// The run sits at the gate of an auto_on_approval stage that already ran.
	task := sqlc.Task{
		ID: "T-aoa", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "paused_gate", PipelinePack: "test@0.1.0",
		CurrentStage: nullStr("gate"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// The advance path reads the prior stage's stored result to resolve the
	// transition; seed one completed invocation.
	store.mu.Lock()
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		ID: "inv-1", TaskID: "T-aoa", Stage: "gate", Sequence: 1,
		Result: toNullRaw([]byte(`{"schema_version":"1","status":"complete","summary":"ok"}`)),
	})
	store.mu.Unlock()

	taskPack := scriptPack("gate", map[string]pack.Stage{
		"gate": {Gate: pack.GateAutoOnApproval, Prompt: "gate.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	manifestFake := &fakeManifestService{}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &scriptAdapter{},
	})
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("advance", "T-aoa", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}

	decisions := systemDecisions(manifestFake)
	if len(decisions) != 1 {
		t.Fatalf("auto_on_approval passing must record exactly one system decision; got %d (%+v)", len(decisions), decisions)
	}
	if decisions[0].Stage != "gate" || decisions[0].Gate != "auto_on_approval" || decisions[0].Decision != "approved" {
		t.Fatalf("decision = %+v, want {gate auto_on_approval approved}", decisions[0])
	}
}

func TestRunner_HumanApprovalAdvanceRecordsNoSystemDecision(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{
		ID: "T-hum", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "paused_gate", PipelinePack: "test@0.1.0",
		CurrentStage: nullStr("plan"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	store.mu.Lock()
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		ID: "inv-1", TaskID: "T-hum", Stage: "plan", Sequence: 1,
		Result: toNullRaw([]byte(`{"schema_version":"1","status":"complete","summary":"ok"}`)),
	})
	store.mu.Unlock()

	taskPack := scriptPack("plan", map[string]pack.Stage{
		"plan": {Gate: pack.GateHumanApproval, Prompt: "plan.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	manifestFake := &fakeManifestService{}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &scriptAdapter{},
	})
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("advance", "T-hum", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	// The human's decision at a human gate is recorded by the API handler in
	// the same transaction as the advance; the runner adding a system record
	// would only add noise to the very section that exists to separate them.
	if decisions := systemDecisions(manifestFake); len(decisions) != 0 {
		t.Fatalf("a human gate records no system decision; got %+v", decisions)
	}
}
