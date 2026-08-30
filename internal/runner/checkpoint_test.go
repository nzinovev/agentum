package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// TestRunner_DeliveryChecksCommitBoundToCheckpoint is the E2 binding test: the
// checks must verify exactly the post-stage checkpoint commit the orchestrator
// created, not a pre-run HEAD read that could be stale. An adapter that writes a
// file (simulating agent work) forces the orchestrator to commit it as a
// checkpoint; the recorded checks.commit must be that new commit, distinct from
// the base. Before E1+E2, checks.commit was whatever HEAD happened to be (often
// the base), tested against a working tree that corresponded to no commit.
func TestRunner_DeliveryChecksCommitBoundToCheckpoint(t *testing.T) {
	t.Parallel()
	repo := seedRepoWithChecks(t,
		"api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n    required: true\n")
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "Tcb", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// writingAdapter writes a file to the worktree — agent work the orchestrator
	// must commit as a checkpoint before checks bind to it.
	adapter := &writingAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}, writeName: "feature.txt", writeBody: "agent work"}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})

	if err := runner.Handle(context.Background(), job("run", "Tcb", "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state = %q, want awaiting_final_review", got)
	}

	// The post-spec checkpoint is a real commit beyond the base (E1), and the
	// branch tip (which becomes result_commit) carries the agent's file. Before
	// E1, both were the base SHA and the diff was empty — the whole defect.
	manager := worktree.New()
	tip, err := manager.ResolveRef(context.Background(), repo, worktree.BranchFor("Tcb"))
	if err != nil {
		t.Fatalf("resolve branch tip: %v", err)
	}
	diff := mustGitRaw(t, repo, "diff", "--name-only", task.BaseCommit.String+".."+tip)
	if !strings.Contains(diff, "feature.txt") {
		t.Fatalf("base..result diff = %q, want feature.txt (the agent's work must be delivered, not discarded)", diff)
	}
}

// TestRunner_DeliveryChecksFailOnDirtyTree is the E2 clean-tree precondition: a
// worktree left dirty at the delivery boundary fails rather than running checks
// against unversioned content. The checks must not claim to have verified a
// commit whose tree they did not test. Reached by calling enforceProjectChecks
// directly on a dirty worktree — the same guard fires in the live flow when
// something writes after the checkpoint.
func TestRunner_DeliveryChecksFailOnDirtyTree(t *testing.T) {
	t.Parallel()
	repo := seedRepoWithChecks(t,
		"api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n    required: true\n")
	manager := worktree.New()
	baseCommit, err := manager.ResolveRef(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	wt, err := manager.Create(context.Background(), repo, "task-dirty", baseCommit)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	// Leave the tree dirty: an uncommitted file the checkpoint did not capture.
	if err := os.WriteFile(filepath.Join(wt.Root, "scratch.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{
		ID: "Td", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", BaseCommit: nullStr(baseCommit),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &scriptAdapter{}, CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})
	run := stageRun{task: task, project: proj, worktree: wt}

	_, _, enforceErr := runner.enforceProjectChecks(context.Background(), run)
	if !errors.Is(enforceErr, ErrDirtyTreeAtDeliveryBoundary) {
		t.Fatalf("enforceProjectChecks on a dirty tree returned %v, want ErrDirtyTreeAtDeliveryBoundary", enforceErr)
	}
}

// TestRunner_DeliveryChecksFailOnMissingBaseCommit is the E4 fail-closed test:
// reaching the delivery boundary without a resolved base_commit fails the task
// rather than routing to an empty set and MandatoryPassed()=true vacuously. This
// is the mirror image of the fail-open defects PR C and PR D fixed — fail-closed
// at the one boundary whose entire purpose is to be fail-closed.
func TestRunner_DeliveryChecksFailOnMissingBaseCommit(t *testing.T) {
	t.Parallel()
	repo := seedRepoWithChecks(t,
		"api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n    required: true\n")
	manager := worktree.New()
	baseCommit, err := manager.ResolveRef(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	wt, err := manager.Create(context.Background(), repo, "task-nobase", baseCommit)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	// Task with NO base_commit — the broken state loadRegistryAtBaseCommit must
	// reject rather than treat as "no registry."
	task := sqlc.Task{
		ID: "Tn", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", // BaseCommit deliberately unset
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	nobasePack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: nobasePack}, Adapter: &scriptAdapter{}, CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})
	run := stageRun{task: task, project: proj, worktree: wt}

	_, _, enforceErr := runner.enforceProjectChecks(context.Background(), run)
	if enforceErr == nil {
		t.Fatal("enforceProjectChecks with no base_commit must fail, not pass vacuously")
	}
	if !strings.Contains(enforceErr.Error(), "base_commit") {
		t.Errorf("error = %v, want one naming base_commit as the missing anchor", enforceErr)
	}
}

// writingAdapter is a scriptAdapter that also writes a file to the worktree,
// simulating an agent that produced work without committing (the normal state,
// since no agent role carries git.delivery). Used to force the orchestrator to
// commit the checkpoint itself.
type writingAdapter struct {
	stubExecution
	scripts   map[string]agent.ResultJSON
	writeName string
	writeBody string
}

func (adapter *writingAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
	}
}

func (adapter *writingAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	// Write the file synchronously before returning the channel so it is on disk
	// when the runner records the checkpoint.
	if adapter.writeName != "" {
		if err := os.WriteFile(filepath.Join(inv.Workdir, adapter.writeName), []byte(adapter.writeBody), 0o644); err != nil {
			return nil, fmt.Errorf("writingAdapter: %w", err)
		}
	}
	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stageOf(inv)]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stageOf(inv))}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stageOf(inv), ResultJSON: scripted}}
	}()
	return eventCh, nil
}

// mustGitRaw runs git in dir and fails the test on error, returning trimmed stdout.
func mustGitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git")
	cmd.Args = append([]string{"git", "-C", dir}, args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRunner_VerifyDeliveryCommitBinding_DivergedRecordsGapAndEvent is the E3
// test: when result_commit (captured at teardown, after human approval) differs
// from the commit the delivery checks verified (body.checks.commit), teardown
// must not silently seal a manifest that asserts "checks passed at X" alongside
// "delivered Y". The divergence is recorded as an evidence gap (so the sealed
// manifest reads incomplete) and emitted as a distinct event (so it is visible
// on the stream). The task is not failed — the human already approved, and
// failing at teardown would be a confusing terminal state.
//
// The verified commit is read directly from body.checks.commit, not proxied
// through the latest checkpoint: the proxy's correctness depends on an FSM
// property (no path back into a stage from the gate) that a future ask-to-edit
// feature would break silently. This test drives the real path by setting the
// manifest's checks commit and asserting the comparison uses it, not a
// checkpoint.
func TestRunner_VerifyDeliveryCommitBinding_DivergedRecordsGapAndEvent(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{
		ID: "Td", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "done",
		PipelinePack: "test@0.1.0",
		// result_commit recorded at teardown, AFTER a human approved and something
		// moved the branch tip (a continue job, an artifact edit, fs access).
		ResultCommit: nullStr("sha-result-tip"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: t.TempDir(), Name: "P"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: scriptPack("spec", nil)}, Adapter: &scriptAdapter{},
	})
	// Inject the fake manifest carrying the verified commit (same-package field
	// assignment, matching evidence_test.go). Drives the primary path: the
	// comparison reads body.checks.commit, not a checkpoint proxy.
	runner.mfst = &fakeManifestService{checksCommitValue: "sha-verified"}

	runner.verifyDeliveryCommitBinding(context.Background(), task)

	// A distinct event names both SHAs so the divergence is visible on the stream.
	found := false
	for _, event := range store.events {
		if event.Type == EvDeliveryCommitDiverged {
			found = true
			payload := string(event.Payload)
			if !strings.Contains(payload, "sha-result-tip") || !strings.Contains(payload, "sha-verified") {
				t.Errorf("divergence event payload = %s, want both SHAs", payload)
			}
		}
	}
	if !found {
		t.Error("no EvDeliveryCommitDiverged event emitted for a result_commit != checks.commit divergence")
	}
}

// TestRunner_VerifyDeliveryCommitBinding_MatchingCommitsIsQuiet is the E3
// negative: when result_commit equals the checks-verified commit, teardown must
// not emit a divergence event or record a gap. The happy path stays quiet so a
// reviewer is not surfaced noise for the normal case.
func TestRunner_VerifyDeliveryCommitBinding_MatchingCommitsIsQuiet(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{
		ID: "Tm", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "done",
		PipelinePack: "test@0.1.0", ResultCommit: nullStr("sha-same"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: t.TempDir(), Name: "P"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: scriptPack("spec", nil)}, Adapter: &scriptAdapter{},
	})
	runner.mfst = &fakeManifestService{checksCommitValue: "sha-same"}

	runner.verifyDeliveryCommitBinding(context.Background(), task)

	for _, event := range store.events {
		if event.Type == EvDeliveryCommitDiverged {
			t.Error("a matching result_commit must not emit a divergence event")
		}
	}
}

// TestRunner_VerifyDeliveryCommitBinding_UnreadableCommitRecordsGap: a
// comparison that could not run must leave a trace. "The check found no
// divergence" and "the check never happened" are different claims about the
// delivery, and a manifest that is silent about both is exactly the fail-open
// shape this comparison was added to close — so a failed read of the verified
// commit records a gap (which drives evidence_complete false) instead of
// returning quietly.
//
// No divergence event is emitted: nothing was compared, so asserting a
// divergence would be a claim we cannot support either.
func TestRunner_VerifyDeliveryCommitBinding_UnreadableCommitRecordsGap(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{
		ID: "Tu", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "done",
		PipelinePack: "test@0.1.0", ResultCommit: nullStr("sha-delivered"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: t.TempDir(), Name: "P"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: scriptPack("spec", nil)}, Adapter: &scriptAdapter{},
	})
	manifestFake := &fakeManifestService{checksCommitErr: errors.New("connection reset by peer")}
	runner.mfst = manifestFake

	runner.verifyDeliveryCommitBinding(context.Background(), task)

	manifestFake.mu.Lock()
	gaps := append([]manifest.EvidenceGap(nil), manifestFake.gaps...)
	manifestFake.mu.Unlock()
	if len(gaps) != 1 {
		t.Fatalf("recorded %d evidence gaps, want exactly 1 for an unreadable verified commit", len(gaps))
	}
	if gaps[0].Section != "checks" {
		t.Errorf("gap section = %q, want checks", gaps[0].Section)
	}
	if !strings.Contains(gaps[0].Reason, "could not compare") {
		t.Errorf("gap reason %q does not say the comparison could not run", gaps[0].Reason)
	}
	for _, event := range store.events {
		if event.Type == EvDeliveryCommitDiverged {
			t.Error("a comparison that never ran must not claim a divergence")
		}
	}
}

// TestRunner_VerifyDeliveryCommitBinding_NoRecordedCommitIsQuiet separates the
// two empty cases the previous test depends on: a task with no recorded checks
// commit (never reached delivery, or the project defines no checks) is an
// absence, not a read failure, and must record nothing.
func TestRunner_VerifyDeliveryCommitBinding_NoRecordedCommitIsQuiet(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{
		ID: "Tn", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "done",
		PipelinePack: "test@0.1.0", ResultCommit: nullStr("sha-delivered"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: t.TempDir(), Name: "P"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: scriptPack("spec", nil)}, Adapter: &scriptAdapter{},
	})
	manifestFake := &fakeManifestService{} // no checks commit, no error
	runner.mfst = manifestFake

	runner.verifyDeliveryCommitBinding(context.Background(), task)

	manifestFake.mu.Lock()
	gapCount := len(manifestFake.gaps)
	manifestFake.mu.Unlock()
	if gapCount != 0 {
		t.Errorf("recorded %d gaps for an absent checks commit; absence is not a failure", gapCount)
	}
	for _, event := range store.events {
		if event.Type == EvDeliveryCommitDiverged {
			t.Error("an absent checks commit must not emit a divergence event")
		}
	}
}

// TestRunner_AutoIfCleanGateFiresOnUndeclaredWrite is the regression test for the
// E1 interaction the initial fix broke. auto_if_clean exists to surface "the
// agent touched files beyond its declared edit_targets," and that is a property
// of the working tree the agent left. recordStageCheckpoint commits that tree,
// so sampling cleanliness AFTER the commit (as the first E1 cut did) makes the
// tree permanently clean and the gate unreachable — the mirror image of the PR B
// defect where isClean was permanently false. This test runs the real loop
// (eval_test cannot catch it: it feeds Clean directly to a pure Evaluate), so a
// writing agent under an auto_if_clean stage must pause for review, not advance.
func TestRunner_AutoIfCleanGateFiresOnUndeclaredWrite(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	// auto_if_clean stage → terminal. The writingAdapter writes an undeclared
	// file, so the gate must pause rather than auto-advance.
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAutoIfClean, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "Tg", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &writingAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}, writeName: "undeclared.txt", writeBody: "the agent touched this"}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
	})

	if err := runner.Handle(context.Background(), job("run", "Tg", "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate — the auto_if_clean gate must fire when the agent writes an undeclared file (sampling cleanliness after the checkpoint commit would make it permanently clean and the gate unreachable)", got)
	}
}

// TestRunner_AutoIfCleanGateAdvancesOnCleanTree is the negative: an auto_if_clean
// stage whose agent wrote nothing (clean tree) advances, not pauses. This pins
// that the fix did not invert the gate — only restored it.
func TestRunner_AutoIfCleanGateAdvancesOnCleanTree(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAutoIfClean, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "Tc", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// scriptAdapter writes nothing — the tree stays clean.
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, CheckExec: checks.NewExecutor(checks.ExecutorDeps{}),
	})

	if err := runner.Handle(context.Background(), job("run", "Tc", "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Clean tree → gate auto-advances → terminal → delivery checks → final gate.
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state = %q, want awaiting_final_review (clean tree under auto_if_clean must advance)", got)
	}
}
