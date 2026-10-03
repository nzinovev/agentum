package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// These tests drive runs against a REAL pack.ProjectSource over a git repo
// carrying a committed project pack: the effective resolution (project layer
// at the pinned base_commit), the policy floor, the pack-drift pause, and the
// no-invocation continue all run against the same plumbing production uses.

// runnerCommitTree adapts the worktree manager to pack.CommitTree. The
// production adapter lives in internal/server; this local copy keeps the
// runner package's tests independent of the server package.
type runnerCommitTree struct{ manager *worktree.Manager }

func (adapter runnerCommitTree) FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error) {
	return adapter.manager.FileAtCommit(ctx, repoPath, commit, path)
}

func (adapter runnerCommitTree) ListTreeAtCommit(ctx context.Context, repoPath, commit, dir string) ([]pack.TreeEntry, error) {
	entries, err := adapter.manager.ListTreeAtCommit(ctx, repoPath, commit, dir)
	if err != nil {
		return nil, err
	}
	mapped := make([]pack.TreeEntry, 0, len(entries))
	for _, entry := range entries {
		mapped = append(mapped, pack.TreeEntry{Path: entry.Path, Mode: entry.Mode, Size: entry.Size})
	}
	return mapped, nil
}

// projectPackFixture drives a run whose pack resolves through a real
// ProjectSource: the repo carries .agentum/packs/probe/ committed at HEAD.
type projectPackFixture struct {
	repo     string
	store    *fakeStore
	runner   *Runner
	source   *pack.ProjectSource
	runID    string
	manifest string
}

// floorPassingProjectPack is a project manifest the floor accepts: an approval
// whose stage stops for a human, capabilities inside the host set, and a
// reachable terminal. No source-writing stage, so rule 1 has nothing to guard.
const floorPassingProjectPack = `api: agentum/v1
pack:
  name: probe
  version: 1.0.0
  persona: engineering
memory: {reads: [project], writes: false}
capabilities: [fs.read]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: fast}
entry: spec
approvals:
  - {name: spec, stage: spec, artifact: spec.md, unlocks: source_write}
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: done
  done: {}
`

// commitProjectPack writes .agentum/packs/probe/ into the repo and commits it.
func commitProjectPack(t *testing.T, repo, manifestBody, promptBody string) string {
	t.Helper()
	packDir := filepath.Join(repo, ".agentum", "packs", "probe", "prompts")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agentum", "packs", "probe", "manifest.yaml"), []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "spec.md"), []byte(promptBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execGit(repo, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := execGit(repo, "commit", "--quiet", "-m", "project pack"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return ""
}

func newProjectPackFixture(t *testing.T, manifestBody string) *projectPackFixture {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	commitProjectPack(t, repo, manifestBody, "spec prompt body")

	runID := "T-pp"
	record := sqlc.Run{ID: runID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "probe"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done"},
	}}
	// A builtin root with no packs: every "probe" resolution must come from
	// the project layer, never from a builtin of the same name.
	source := pack.NewProjectSource(pack.NewDirSource(t.TempDir()), runnerCommitTree{manager: worktree.New()})
	runner := New(Deps{Store: store, Packs: source, Adapter: adapter})
	return &projectPackFixture{repo: repo, store: store, runner: runner, source: source, runID: runID, manifest: manifestBody}
}

// TestRunner_ProjectPackResolvesAndPinsOrigin: the run resolves its pack from
// the project layer at the pinned base_commit (origin "project" pinned
// resolve-once) and drives to the pack's own first gate.
func TestRunner_ProjectPackResolvesAndPinsOrigin(t *testing.T) {
	t.Parallel()
	fixture := newProjectPackFixture(t, floorPassingProjectPack)
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate at the pack's human gate", got)
	}
	if !fixture.store.record.PipelinePackOrigin.Valid || fixture.store.record.PipelinePackOrigin.String != string(pack.OriginProject) {
		t.Fatalf("pipeline_pack_origin = %+v, want project", fixture.store.record.PipelinePackOrigin)
	}
	if got := len(fixture.store.invocations); got != 1 {
		t.Fatalf("invocations = %d, want 1 (the project pack's spec stage)", got)
	}
}

// TestRunner_PackFromLaterCommitDoesNotAffectRunningRun: a pack committed
// AFTER the run pinned its base_commit changes nothing the run executes — the
// effective pack is read from the pinned commit, so the continuation still
// runs the stage graph the run started with.
func TestRunner_PackFromLaterCommitDoesNotAffectRunningRun(t *testing.T) {
	t.Parallel()
	fixture := newProjectPackFixture(t, floorPassingProjectPack)
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate before the pack change", got)
	}

	// Commit a pack whose entry stage is RENAMED — a run that picked the new
	// manifest up would fail on a stage its paused position does not know.
	renamed := strings.Replace(floorPassingProjectPack, "entry: spec", "entry: spec2", 1)
	renamed = strings.Replace(renamed, "  spec:\n", "  spec2:\n", 1)
	commitProjectPack(t, fixture.repo, renamed, "changed prompt")

	if _, stateErr := fixture.store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: fixture.runID, TenantID: "tn", State: "running",
	}); stateErr != nil {
		t.Fatal(stateErr)
	}
	continueJob := sqlc.Job{Kind: "continue", RunID: fixture.runID, TenantID: "tn", UserID: "us", Payload: json.RawMessage(`{}`)}
	if err := fixture.runner.HandleContinue(t.Context(), continueJob); err != nil {
		t.Fatalf("continue after a later pack commit must not fail: %v", err)
	}
	if got := fixture.store.taskState(); got == "failed" {
		t.Fatal("the run must keep executing the base_commit pack, not fail on the new graph")
	}
}

// TestRunner_ProjectPackDriftPausesAndLiftsOnRevert: an uncommitted edit in
// the run's pack directory pauses the run before any work; reverting the edit
// and continuing starts the first stage. The committed-difference variant
// (HEAD moved off base_commit) never pauses — the run keeps executing the
// pinned base_commit pack.
func TestRunner_ProjectPackDriftPausesAndLiftsOnRevert(t *testing.T) {
	t.Parallel()
	fixture := newProjectPackFixture(t, floorPassingProjectPack)
	// An uncommitted edit to the committed manifest: porcelain " M".
	edited := strings.Replace(floorPassingProjectPack, "fix_cycles: 1", "fix_cycles: 2", 1)
	if err := os.WriteFile(filepath.Join(fixture.repo, ".agentum", "packs", "probe", "manifest.yaml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (project_pack_drift)", got)
	}
	foundDriftEvent := false
	for _, event := range fixture.store.events {
		if event.Type == EvProjectPackDrift {
			foundDriftEvent = true
		}
	}
	if !foundDriftEvent {
		t.Fatal("the drift pause must emit run.project_pack_drift")
	}
	if got := len(fixture.store.invocations); got != 0 {
		t.Fatalf("invocations = %d, want 0 (no stage runs over a drifted pack dir)", got)
	}

	// Reverting the edit clears the drift; a textless continue starts the run.
	if err := os.WriteFile(filepath.Join(fixture.repo, ".agentum", "packs", "probe", "manifest.yaml"), []byte(floorPassingProjectPack), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stateErr := fixture.store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: fixture.runID, TenantID: "tn", State: "running",
	}); stateErr != nil {
		t.Fatal(stateErr)
	}
	continueJob := sqlc.Job{Kind: "continue", RunID: fixture.runID, TenantID: "tn", UserID: "us", Payload: json.RawMessage(`{}`)}
	if err := fixture.runner.HandleContinue(t.Context(), continueJob); err != nil {
		t.Fatalf("continue after revert: %v", err)
	}
	if got := len(fixture.store.invocations); got != 1 {
		t.Fatalf("invocations = %d, want 1 (the first stage starts after the lift)", got)
	}
	if got := fixture.store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate at the spec gate", got)
	}
}

// TestRunner_ProjectPackDriftLiftsOnCommit: committing the edit also clears
// the pause — the working copy is clean against its HEAD again, and the run
// continues on the pinned base_commit pack (the committed difference is the
// documented model, recorded as evidence, never a stop).
func TestRunner_ProjectPackDriftLiftsOnCommit(t *testing.T) {
	t.Parallel()
	fixture := newProjectPackFixture(t, floorPassingProjectPack)
	edited := strings.Replace(floorPassingProjectPack, "fix_cycles: 1", "fix_cycles: 2", 1)
	if err := os.WriteFile(filepath.Join(fixture.repo, ".agentum", "packs", "probe", "manifest.yaml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want the drift pause before the commit", got)
	}

	if _, err := execGit(fixture.repo, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := execGit(fixture.repo, "commit", "--quiet", "-m", "commit pack edit"); err != nil {
		t.Fatal(err)
	}
	if _, stateErr := fixture.store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: fixture.runID, TenantID: "tn", State: "running",
	}); stateErr != nil {
		t.Fatal(stateErr)
	}
	continueJob := sqlc.Job{Kind: "continue", RunID: fixture.runID, TenantID: "tn", UserID: "us", Payload: json.RawMessage(`{}`)}
	if err := fixture.runner.HandleContinue(t.Context(), continueJob); err != nil {
		t.Fatalf("continue after commit: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate (a committed difference never pauses)", got)
	}
	if got := len(fixture.store.invocations); got != 1 {
		t.Fatalf("invocations = %d, want 1", got)
	}
}

// TestRunner_ProjectPackDriftIgnoredJunkDoesNotPause: an ignored file that is
// not the pack's manifest or overrides document is recorded, not paused on —
// editor junk under global excludes must not hold every run hostage.
func TestRunner_ProjectPackDriftIgnoredJunkDoesNotPause(t *testing.T) {
	t.Parallel()
	fixture := newProjectPackFixture(t, floorPassingProjectPack)
	// A committed ignore rule, then the junk file it hides.
	if err := os.WriteFile(filepath.Join(fixture.repo, ".gitignore"), []byte(".DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execGit(fixture.repo, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := execGit(fixture.repo, "commit", "--quiet", "-m", "gitignore"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, ".agentum", "packs", "probe", ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want the run driving to its gate despite ignored junk", got)
	}
}

// TestRunner_PolicyFloorRejectsUnapprovedProjectPack: a project pack whose
// implement stage source-writes with no source_write approval fails the run
// before any work, naming rule 1 and the layer the offending graph came from.
func TestRunner_PolicyFloorRejectsUnapprovedProjectPack(t *testing.T) {
	t.Parallel()
	unapproved := `api: agentum/v1
pack:
  name: probe
  version: 1.0.0
  persona: engineering
memory: {reads: [project], writes: false}
capabilities: [fs.read, fs.write]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: fast}
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: implement
  implement:
    gate: auto
    role: implementer
    prompt: prompts/spec.md
    transitions:
      - to: done
  done: {}
`
	fixture := newProjectPackFixture(t, unapproved)
	if err := fixture.runner.HandleRun(t.Context(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := fixture.store.taskState(); got != "failed" {
		t.Fatalf("state = %q, want failed (the floor rejects before any work)", got)
	}
	var floorPayload string
	foundFloorEvent := false
	for _, event := range fixture.store.events {
		if event.Type == EvPackFloorViolated {
			foundFloorEvent = true
			floorPayload = string(event.Payload)
		}
	}
	if !foundFloorEvent {
		t.Fatal("the floor rejection must emit run.pack_floor_violation")
	}
	if !strings.Contains(floorPayload, "approval_precedes_source_write") {
		t.Fatalf("event payload must name rule 1: %s", floorPayload)
	}
	if !strings.Contains(floorPayload, "project pack") {
		t.Fatalf("event payload must name the layer the offending graph came from: %s", floorPayload)
	}
	if got := len(fixture.store.invocations); got != 0 {
		t.Fatalf("invocations = %d, want 0 (no stage runs under a floor violation)", got)
	}
}
