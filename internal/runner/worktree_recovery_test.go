package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
)

// The failure-preservation contract (stage 1 of the run-recovery plan): an
// automatic failure never destroys the working copy, a crashed run's dirty
// tree never gets wiped without a human decision, and every explicit recovery
// or discard action is verified against the exact tree it was made about.
// These tests drive the real git worktree path — no DB, the fake store —
// because the guarantee being pinned is about files on disk.

// recoveryFixture bundles one repo, run, and runner wired for the recovery
// tests. The pack is spec(human_approval)→impl(auto)→done, so one HandleRun
// leaves the run at paused_gate over a live worktree — the state from which a
// crashed-and-resumed run presents uncommitted work to the reconciler.
type recoveryFixture struct {
	repo   string
	store  *fakeStore
	runner *Runner
	runID  string
}

func newRecoveryFixture(t *testing.T, runState string) *recoveryFixture {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runID := "T-rec"
	record := sqlc.Run{ID: runID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: runState, PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		"impl": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter,
		CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})
	return &recoveryFixture{repo: repo, store: store, runner: runner, runID: runID}
}

// worktreeRoot resolves the run's worktree path inside the fixture repo.
func (fixture *recoveryFixture) worktreeRoot() string {
	return worktree.PathFor(fixture.repo, fixture.runID)
}

// driveToGate runs the run job so the worktree is created and the spec stage
// pauses at its human gate.
func (fixture *recoveryFixture) driveToGate(t *testing.T) {
	t.Helper()
	if err := fixture.runner.HandleRun(context.Background(), job("run", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("drive run: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("run state = %q, want paused_gate", state)
	}
}

// resumeAsRunning applies the state change a lifecycle handler makes when the
// human answers a pause (paused → running), so the driving job the tests then
// fire enters under the same conditions production presents.
func (fixture *recoveryFixture) resumeAsRunning(t *testing.T) {
	t.Helper()
	if _, stateErr := fixture.store.UpdateRunState(context.Background(), sqlc.UpdateRunStateParams{
		ID: fixture.runID, TenantID: "tn", State: "running",
	}); stateErr != nil {
		t.Fatal(stateErr)
	}
}

// pauseForDirtyTree is the shared prefix of the reconcile tests: dirty the
// worktree, resume as running, and fire a continue job, which must land in
// paused_user_stop with stop_reason worktree_uncommitted_changes and the dirty
// file untouched.
func (fixture *recoveryFixture) pauseForDirtyTree(t *testing.T, dirtyRelPath, dirtyContent string) {
	t.Helper()
	wtRoot := fixture.worktreeRoot()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(wtRoot, dirtyRelPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtRoot, dirtyRelPath), []byte(dirtyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("continue over dirty tree: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop", state)
	}
	got, readErr := os.ReadFile(filepath.Join(wtRoot, dirtyRelPath))
	if readErr != nil || string(got) != dirtyContent {
		t.Fatalf("uncommitted file did not survive the reconcile pause: err=%v content=%q", readErr, string(got))
	}
	requireRecoveryEvent(t, fixture.store, EvWorktreeRecoveryRequired)
}

// reconcileJobFor builds the reconcile job carrying the decision, after
// applying the paused→running transition its endpoint performs.
func (fixture *recoveryFixture) reconcileJobFor(t *testing.T, decision taskinput.ReconcileDecision, jobID int64) sqlc.Job {
	t.Helper()
	payload, marshalErr := decision.Marshal()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	fixture.resumeAsRunning(t)
	return sqlc.Job{ID: jobID, TenantID: "tn", UserID: "us", RunID: fixture.runID, Kind: "reconcile", Payload: payload}
}

// TestRunner_FailureKeepsWorktreeAndUncommittedFiles pins the core promise:
// failRun transitions to failed WITHOUT enqueueing teardown, so the worktree
// and its untracked files survive the failure for a human to salvage. A
// mandatory check failure is the failure the runner itself produces.
func TestRunner_FailureKeepsWorktreeAndUncommittedFiles(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	// A registry whose mandatory check always fails, committed at HEAD so the
	// base_commit read finds it.
	if err := os.WriteFile(filepath.Join(repo, ".agentum.yaml"),
		[]byte("api: agentum/v1\nchecks:\n  - name: build\n    command: [\"false\"]\n    required: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitAll(repo, "add failing checks"); err != nil {
		t.Fatalf("commit checks: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runID := "T-failkeep"
	record := sqlc.Run{ID: runID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter,
		CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})

	handleErr := runner.HandleRun(context.Background(), job("run", runID, "tn", "us"))
	if handleErr == nil {
		t.Fatal("expected the mandatory check failure to surface as a Handle error")
	}
	if state := store.taskState(); state != "failed" {
		t.Fatalf("state = %q, want failed", state)
	}
	// No teardown job was enqueued by the failure.
	for _, kind := range store.enqueued {
		if kind == "teardown" {
			t.Fatal("failRun enqueued a teardown job; a system failure must not destroy the working copy")
		}
	}
	// The worktree and its branch survive.
	wtRoot := worktree.PathFor(repo, runID)
	if !worktree.DirPresent(wtRoot) {
		t.Fatal("worktree was removed after failure")
	}
	if out, err := execGit(repo, "rev-parse", "--verify", worktree.BranchFor(runID)); err != nil {
		t.Fatalf("branch missing after failure: %v (%s)", err, out)
	}
}

// TestRunner_DirtyWorktreePausesInsteadOfRestoring pins the reconcile change:
// a worktree holding uncommitted changes pauses with a diagnostic instead of
// the old automatic reset --hard + clean -fd. The dirty file must still be on
// disk, byte for byte, after the driving job returns.
func TestRunner_DirtyWorktreePausesInsteadOfRestoring(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	fixture.pauseForDirtyTree(t, "notes/uncommitted.txt", "half-finished agent work that must survive")
}

// TestRunner_ReconcileKeepAsCheckpointCommitsTheTree drives the human
// decision "keep": the dirty tree becomes an orchestrator-authored checkpoint
// and the run resumes; the file's content is reachable from the branch.
func TestRunner_ReconcileKeepAsCheckpointCommitsTheTree(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	const dirtyPath = "notes/kept.txt"
	const dirtyContent = "kept by the human recovery decision"
	fixture.pauseForDirtyTree(t, dirtyPath, dirtyContent)

	head, headErr := execGit(fixture.worktreeRoot(), "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("read worktree head: %v", headErr)
	}
	reconcileJob := fixture.reconcileJobFor(t, taskinput.ReconcileDecision{
		Mode: taskinput.ReconcileKeepAsCheckpoint, ExpectedHead: head,
	}, 7)
	if err := fixture.runner.HandleReconcile(context.Background(), reconcileJob); err != nil {
		t.Fatalf("reconcile keep: %v", err)
	}
	// The spec stage re-ran (resumed session) and paused at its human gate
	// again — the loop closed over the kept tree.
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate", state)
	}
	if out, err := execGit(fixture.worktreeRoot(), "show", "HEAD:"+dirtyPath); err != nil || out != dirtyContent {
		t.Fatalf("kept file not in the checkpoint commit: err=%v content=%q", err, out)
	}
	requireRecoveryEvent(t, fixture.store, EvWorktreeReconcileDecided)
}

// TestRunner_ReconcileDiscardRestoresTheCheckpoint drives the human decision
// "discard": the tree resets to the last checkpoint, the dirty file is gone,
// and the run resumes.
func TestRunner_ReconcileDiscardRestoresTheCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	fixture.pauseForDirtyTree(t, "thrown-away.txt", "this work is deliberately discarded")

	head, headErr := execGit(fixture.worktreeRoot(), "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("read worktree head: %v", headErr)
	}
	reconcileJob := fixture.reconcileJobFor(t, taskinput.ReconcileDecision{
		Mode: taskinput.ReconcileDiscardToCheckpoint, ExpectedHead: head, ConfirmUncommittedLoss: true,
	}, 8)
	if err := fixture.runner.HandleReconcile(context.Background(), reconcileJob); err != nil {
		t.Fatalf("reconcile discard: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.worktreeRoot(), "thrown-away.txt")); !os.IsNotExist(statErr) {
		t.Fatal("discarded file still present after the discard decision")
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate", state)
	}
}

// TestRunner_ReconcileResumeSessionKeepsTheTree drives the human decision
// "resume": nothing on disk changes, and the run proceeds over the tree as it
// stands — the explicit allowance the pause was waiting for.
func TestRunner_ReconcileResumeSessionKeepsTheTree(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	const dirtyContent = "left exactly as the session had it"
	fixture.pauseForDirtyTree(t, "resume-me.txt", dirtyContent)

	head, headErr := execGit(fixture.worktreeRoot(), "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("read worktree head: %v", headErr)
	}
	reconcileJob := fixture.reconcileJobFor(t, taskinput.ReconcileDecision{
		Mode: taskinput.ReconcileResumeSession, ExpectedHead: head,
	}, 9)
	if err := fixture.runner.HandleReconcile(context.Background(), reconcileJob); err != nil {
		t.Fatalf("reconcile resume: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(fixture.worktreeRoot(), "resume-me.txt"))
	if readErr != nil || string(got) != dirtyContent {
		t.Fatalf("resume decision must not touch the tree: err=%v content=%q", readErr, string(got))
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate", state)
	}
}

// TestRunner_ReconcileRefusesMovedHead pins the guard that keeps a decision
// from applying to bytes it was not made about: when HEAD moved between the
// decision and its application, the job refuses, nothing changes on disk, and
// the refusal is on the event log.
func TestRunner_ReconcileRefusesMovedHead(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	fixture.pauseForDirtyTree(t, "dirty.txt", "dirt")

	reconcileJob := fixture.reconcileJobFor(t, taskinput.ReconcileDecision{
		Mode:                   taskinput.ReconcileDiscardToCheckpoint,
		ExpectedHead:           strings.Repeat("0", 40), // not the real HEAD
		ConfirmUncommittedLoss: true,
	}, 10)
	if err := fixture.runner.HandleReconcile(context.Background(), reconcileJob); err == nil {
		t.Fatal("expected a HEAD-mismatch refusal")
	}
	if _, statErr := os.Stat(filepath.Join(fixture.worktreeRoot(), "dirty.txt")); statErr != nil {
		t.Fatalf("refused decision must not touch the tree: %v", statErr)
	}
	requireRecoveryEvent(t, fixture.store, EvWorktreeReconcileRefused)
}

// TestRunner_DiscardWorktreeGuardsAndRemoval covers the explicit disposal
// action: a dirty tree without confirmation is refused, the wrong HEAD is
// refused, a competing job is refused, and a confirmed request removes the
// worktree while the branch and its commits survive.
func TestRunner_DiscardWorktreeGuardsAndRemoval(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)

	wtRoot := fixture.worktreeRoot()
	const dirtyContent = "uncommitted work the human agreed to lose"
	if err := os.WriteFile(filepath.Join(wtRoot, "to-lose.txt"), []byte(dirtyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	head, headErr := execGit(wtRoot, "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("read worktree head: %v", headErr)
	}
	branchTipBefore, tipErr := execGit(fixture.repo, "rev-parse", worktree.BranchFor(fixture.runID))
	if tipErr != nil {
		t.Fatalf("read branch tip: %v", tipErr)
	}
	discardJob := func(payload string) sqlc.Job {
		return sqlc.Job{ID: 11, TenantID: "tn", UserID: "us", RunID: fixture.runID, Kind: "discard_worktree", Payload: []byte(payload)}
	}

	// Dirty tree, no confirmation: refused, tree intact.
	if err := fixture.runner.HandleDiscardWorktree(context.Background(),
		discardJob(`{"expected_head":"`+head+`","discard_uncommitted":false}`)); err == nil {
		t.Fatal("expected the unconfirmed dirty discard to be refused")
	}
	if !worktree.DirPresent(wtRoot) {
		t.Fatal("refused discard must not remove the worktree")
	}
	requireRecoveryEvent(t, fixture.store, EvWorktreeDiscardRefused)

	// Wrong HEAD: refused.
	if err := fixture.runner.HandleDiscardWorktree(context.Background(),
		discardJob(`{"expected_head":"`+strings.Repeat("1", 40)+`","discard_uncommitted":true}`)); err == nil {
		t.Fatal("expected the wrong-HEAD discard to be refused")
	}
	if !worktree.DirPresent(wtRoot) {
		t.Fatal("refused discard must not remove the worktree")
	}

	// Competing job in flight: refused (the fake store scripts one).
	fixture.store.unfinishedJobs = 1
	if err := fixture.runner.HandleDiscardWorktree(context.Background(),
		discardJob(`{"expected_head":"`+head+`","discard_uncommitted":true}`)); err == nil {
		t.Fatal("expected the jobs-in-flight discard to be refused")
	}
	fixture.store.unfinishedJobs = 0

	// Confirmed, right HEAD, no competing job: the tree goes, the branch stays.
	if err := fixture.runner.HandleDiscardWorktree(context.Background(),
		discardJob(`{"expected_head":"`+head+`","discard_uncommitted":true}`)); err != nil {
		t.Fatalf("confirmed discard: %v", err)
	}
	if worktree.DirPresent(wtRoot) {
		t.Fatal("worktree still present after a confirmed discard")
	}
	branchTipAfter, tipErr := execGit(fixture.repo, "rev-parse", worktree.BranchFor(fixture.runID))
	if tipErr != nil {
		t.Fatalf("branch missing after discard: %v", tipErr)
	}
	if branchTipAfter != branchTipBefore {
		t.Fatalf("branch tip moved during discard: before %s after %s", branchTipBefore, branchTipAfter)
	}
	requireRecoveryEvent(t, fixture.store, EvWorktreeDiscarded)
}

// TestRunner_DiscardWorktreeRefusesLiveRun pins the state guard on the runner
// side: a run the API let through (say it raced a start) is refused at
// execution time when its state says running.
func TestRunner_DiscardWorktreeRefusesLiveRun(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	// Simulate the API-side check passing and a start racing in before the
	// job executes: the run is running again.
	fixture.resumeAsRunning(t)
	wtRoot := fixture.worktreeRoot()
	head, headErr := execGit(wtRoot, "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("read worktree head: %v", headErr)
	}
	discardJob := sqlc.Job{ID: 12, TenantID: "tn", UserID: "us", RunID: fixture.runID, Kind: "discard_worktree",
		Payload: []byte(`{"expected_head":"` + head + `","discard_uncommitted":true}`)}
	if err := fixture.runner.HandleDiscardWorktree(context.Background(), discardJob); err == nil {
		t.Fatal("expected the live-run discard to be refused")
	}
	if !worktree.DirPresent(wtRoot) {
		t.Fatal("refused discard removed the worktree anyway")
	}
}

// requireRecoveryEvent asserts one event of the given type reached the
// durable log, failing with the recorded types otherwise.
func requireRecoveryEvent(t *testing.T, store *fakeStore, eventType string) {
	t.Helper()
	for _, event := range store.events {
		if event.Type == eventType {
			return
		}
	}
	types := make([]string, 0, len(store.events))
	for _, event := range store.events {
		types = append(types, event.Type)
	}
	t.Fatalf("event %q not recorded; got %v", eventType, types)
}

// TestRecoveryEventsCarryPayloads sanity-checks the diagnostic payload of the
// recovery-required event: the HEAD and the dirty entries a human reads to
// decide. A pause without its diagnosis is a dead end for the operator.
func TestRecoveryEventsCarryPayloads(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	if err := os.WriteFile(filepath.Join(fixture.worktreeRoot(), "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	for _, event := range fixture.store.events {
		if event.Type != EvWorktreeRecoveryRequired {
			continue
		}
		if decodeErr := json.Unmarshal(event.Payload, &payload); decodeErr != nil {
			t.Fatalf("recovery payload is not JSON: %v", decodeErr)
		}
	}
	if payload == nil {
		t.Fatal("no recovery-required event recorded")
	}
	if head, _ := payload["head"].(string); head == "" {
		t.Fatal("recovery payload carries no HEAD")
	}
	entries, _ := payload["dirty_entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("dirty_entries = %v, want the one dirty path", payload["dirty_entries"])
	}
}
