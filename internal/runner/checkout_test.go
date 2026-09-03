package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/repoid"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// The tests in this file pin the run's relationship to its working copy: a run
// executes in the copy it pinned at start (never in whatever copy the project
// happens to point at now), and a copy that is unavailable or foreign pauses
// the run instead of letting it rebuild its lineage somewhere else. They use
// real git repositories in temp directories because the whole point is what
// git says about the paths.

// cloneRepo clones source into destination (a fresh directory), so two
// fixtures can share one history — two working copies of one repository.
func cloneRepo(t *testing.T, source, destination string) {
	t.Helper()
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir clone parent: %v", err)
	}
	cmd := exec.Command("git", "clone", "--quiet", source, destination)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone %s -> %s: %v (%s)", source, destination, err, out)
	}
}

// TestRunner_RunExecutesInPinnedCheckoutNotProjectPath: the project was
// re-registered from a second clone (repo_path points there), but the run
// pinned the first copy — the continuation must build its worktree in the
// pinned copy, where its branch and checkpoints live, not in the project's
// current one. Both clones carry the same repository identity, so identity
// verification alone would happily let the run wander; only the pin holds it.
func TestRunner_RunExecutesInPinnedCheckoutNotProjectPath(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	if err := initRepoWithCommit(source); err != nil {
		t.Fatalf("setup source repo: %v", err)
	}
	clonesParent := t.TempDir()
	originalCopy := filepath.Join(clonesParent, "original")
	secondCopy := filepath.Join(clonesParent, "second")
	cloneRepo(t, source, originalCopy)
	cloneRepo(t, source, secondCopy)

	// The run pinned the original copy; the project now points at the second.
	task := sqlc.Task{
		ID: "T-pin", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CheckoutPath: originalCopy,
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: secondCopy, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	if err := runner.Handle(t.Context(), job("run", "T-pin", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}

	pinnedWorktree := worktree.PathFor(originalCopy, "T-pin")
	if !worktree.DirPresent(pinnedWorktree) {
		t.Fatalf("worktree must live in the pinned copy %s", pinnedWorktree)
	}
	if worktree.DirPresent(worktree.PathFor(secondCopy, "T-pin")) {
		t.Fatal("a worktree was created in the project's current copy — the run left its pinned copy")
	}
}

// TestRunner_UnavailableCheckoutPausesWithoutWorktree: the pinned copy is gone
// (moved away, deleted). The run pauses with stop_reason checkout_unavailable
// naming the path — it does not fail, and above all it does not create a
// worktree anywhere: recreating one in some other copy is the silent loss of
// the run's whole commit line.
func TestRunner_UnavailableCheckoutPausesWithoutWorktree(t *testing.T) {
	t.Parallel()

	goneCopy := filepath.Join(t.TempDir(), "gone")
	task := sqlc.Task{
		ID: "T-gone", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CheckoutPath: goneCopy,
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: goneCopy, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &scriptAdapter{},
	})

	if err := runner.Handle(t.Context(), job("run", "T-gone", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}

	if got := store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (a pause, not a failure)", got)
	}
	store.mu.Lock()
	invocationCount := len(store.invocations)
	events := append([]sqlc.Event(nil), store.events...)
	store.mu.Unlock()
	if invocationCount != 0 {
		t.Fatalf("no stage may run while the working copy is unavailable; got %d invocations", invocationCount)
	}
	var unavailableEvent *sqlc.Event
	for index := range events {
		if events[index].Type == EvCheckoutUnavailable {
			unavailableEvent = &events[index]
			break
		}
	}
	if unavailableEvent == nil {
		t.Fatalf("no %s event was emitted", EvCheckoutUnavailable)
	}
	if !strings.Contains(string(unavailableEvent.Payload), goneCopy) {
		t.Fatalf("the event must name the unavailable path %q: %s", goneCopy, unavailableEvent.Payload)
	}
	if unavailableEvent.Actor != string(authz.ActorSystem) {
		t.Fatalf("event actor = %q, want system", unavailableEvent.Actor)
	}
}

// TestRunner_ForeignCheckoutPauses: the path resolves to a repository, but a
// DIFFERENT one than the project was registered as. Same pause as an
// unavailable copy — a usable-looking tree with the wrong history is the most
// dangerous variant, because work would continue and look correct.
func TestRunner_ForeignCheckoutPauses(t *testing.T) {
	t.Parallel()

	registeredRepo := t.TempDir()
	if err := initRepoWithCommit(registeredRepo); err != nil {
		t.Fatalf("setup registered repo: %v", err)
	}
	foreignRepo := t.TempDir()
	// The marker lands in the root commit itself: two fixtures built in the
	// same second with identical content hash to the SAME root commit and
	// therefore the same identity, and a foreign repository must genuinely be
	// foreign.
	initRepoWithMarker(t, foreignRepo, "foreign")

	// The project's identity comes from the registered repository; its path
	// was somehow pointed at the foreign one.
	task := sqlc.Task{
		ID: "T-foreign", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: foreignRepo, Name: "P"}
	store := newFakeStore(task, proj)
	store.mu.Lock()
	registeredIdentity, err := repoIdentityOf(registeredRepo)
	if err != nil {
		store.mu.Unlock()
		t.Fatalf("resolve registered identity: %v", err)
	}
	// The row carries the registration facts of the registered repository —
	// identity and root commits — while its path points at the foreign one.
	store.project.RepoIdentity = registeredIdentity.Value
	store.project.RepoRootCommits = registeredIdentity.Roots
	store.mu.Unlock()

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &scriptAdapter{},
	})

	if err := runner.Handle(t.Context(), job("run", "T-foreign", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop — a foreign copy must pause, not run", got)
	}
	store.mu.Lock()
	pauseEvents := 0
	for _, event := range store.events {
		if event.Type == EvCheckoutUnavailable {
			pauseEvents++
		}
	}
	store.mu.Unlock()
	if pauseEvents == 0 {
		t.Fatalf("a foreign copy must emit %s naming the path", EvCheckoutUnavailable)
	}
	if worktree.DirPresent(worktree.PathFor(foreignRepo, "T-foreign")) {
		entries, _ := os.ReadDir(worktree.PathFor(foreignRepo, "T-foreign"))
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("a worktree was created in the foreign copy (entries: %v)", names)
	}
}

// TestRunner_RepoMoveContinuesInMovedCopy: the repository moved on disk and
// the registration carried the run's checkout with it (checkout_path was
// rebound). The continuation must work in the new location: git's absolute
// worktree links went stale with the move, repair rewires them, and the run
// proceeds on its own branch instead of failing or rebuilding.
func TestRunner_RepoMoveContinuesInMovedCopy(t *testing.T) {
	t.Parallel()

	original := t.TempDir()
	if err := initRepoWithCommit(original); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskID := "T-move"
	task := sqlc.Task{
		ID: taskID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: original, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	// First run: creates the worktree and pauses at the human gate.
	if err := runner.Handle(t.Context(), job("run", taskID, "tn", "us")); err != nil {
		t.Fatalf("initial run: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("initial state = %q, want paused_gate", got)
	}

	// The repository (worktrees included) moves; the registration rebinds the
	// run's checkout path to the new location.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move repo: %v", err)
	}
	store.mu.Lock()
	store.project.RepoPath = moved
	store.task.CheckoutPath = moved
	store.mu.Unlock()

	// The advance job continues the run in the moved copy.
	if err := runner.Handle(t.Context(), job("advance", taskID, "tn", "us")); err != nil {
		t.Fatalf("advance after move: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state after move+advance = %q, want awaiting_final_review", got)
	}
	movedWorktree := worktree.PathFor(moved, taskID)
	if !worktree.DirPresent(movedWorktree) {
		t.Fatal("the run's worktree must be usable in the moved copy")
	}
}

// initRepoWithMarker builds a git work tree whose ROOT commit is unique to
// the marker, so its repository identity differs from any other fixture's.
func initRepoWithMarker(t *testing.T, dir, marker string) {
	t.Helper()
	runGitInTest(t, dir, "init", "--quiet", "--initial-branch=main")
	runGitInTest(t, dir, "config", "user.email", "test@agentum")
	runGitInTest(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("# "+marker+"\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitInTest(t, dir, "add", "README")
	runGitInTest(t, dir, "commit", "--quiet", "-m", "init "+marker)
}

// runGitInTest runs git in dir, failing the test on error.
func runGitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
}

// repoIdentityOf resolves a repository's identity outside the fake store.
func repoIdentityOf(repoPath string) (repoid.Identity, error) {
	return repoid.Resolve(context.Background(), repoPath)
}

// TestRunner_TeardownAfterRepoMoveRemovesWorktree: the repository moved after
// the run paused, and the terminal teardown must still clean up in the moved
// copy — relink first, then remove. Without the relink the removal is a
// silent no-op and the worktree outlives the run, potentially holding the
// agent's uncommitted work.
func TestRunner_TeardownAfterRepoMoveRemovesWorktree(t *testing.T) {
	t.Parallel()

	original := t.TempDir()
	if err := initRepoWithCommit(original); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskID := "T-teardown-move"
	task := sqlc.Task{
		ID: taskID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: original, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	if err := runner.Handle(t.Context(), job("run", taskID, "tn", "us")); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move repo: %v", err)
	}
	store.mu.Lock()
	store.project.RepoPath = moved
	store.task.CheckoutPath = moved
	store.task.State = "done"
	store.mu.Unlock()

	if err := runner.Handle(t.Context(), job("teardown", taskID, "tn", "us")); err != nil {
		t.Fatalf("teardown after move: %v", err)
	}
	if worktree.DirPresent(worktree.PathFor(moved, taskID)) {
		t.Fatal("teardown left the worktree behind in the moved copy")
	}
}

// TestRunner_UnrepairableWorktreePauses: the worktree directory is present
// but git cannot relink it (its admin metadata under .git/worktrees was
// pruned by gc or removed by hand). That is an externally fixable state of
// the same class as a missing copy, so the run pauses — a failure would
// destroy the recovery path (restore the metadata, or tear the worktree down
// deliberately) that a pause keeps. And no worktree is ever rebuilt in some
// other copy.
func TestRunner_UnrepairableWorktreePauses(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskID := "T-unrepairable"
	task := sqlc.Task{
		ID: taskID, TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateHumanApproval, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec"},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	if err := runner.Handle(t.Context(), job("run", taskID, "tn", "us")); err != nil {
		t.Fatalf("initial run: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("initial state = %q, want paused_gate", got)
	}

	// The advance handler transitions paused_gate -> running before enqueuing
	// the job; mirror that, as the real flow delivers it.
	store.mu.Lock()
	store.task.State = "running"
	store.mu.Unlock()

	// Prune the worktree's admin metadata: the directory and its .git file
	// remain, but git can neither enter nor relink it.
	if err := os.RemoveAll(filepath.Join(repo, ".git", "worktrees", taskID)); err != nil {
		t.Fatalf("prune worktree metadata: %v", err)
	}

	if err := runner.Handle(t.Context(), job("advance", taskID, "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop — an unrepairable worktree pauses instead of failing", got)
	}
	store.mu.Lock()
	pauseEvents := 0
	for _, event := range store.events {
		if event.Type == EvCheckoutUnavailable {
			pauseEvents++
		}
	}
	store.mu.Unlock()
	if pauseEvents == 0 {
		t.Fatalf("an unrepairable worktree must surface via %s", EvCheckoutUnavailable)
	}
	// No worktree was rebuilt: the original directory is still there (stale)
	// and git's worktree records were not recreated.
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "worktrees", taskID)); !os.IsNotExist(statErr) {
		t.Fatalf("worktree records reappeared: stat err = %v", statErr)
	}
}
