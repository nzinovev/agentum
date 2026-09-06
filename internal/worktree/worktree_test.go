package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchFor_PathFor_ArtifactDir(t *testing.T) {
	t.Parallel()
	if got := BranchFor("abc-123"); got != "agentum/abc-123" {
		t.Errorf("BranchFor = %q", got)
	}
	if got := PathFor("/repo", "abc-123"); got != filepath.Join("/repo", ".agentum", "worktrees", "abc-123") {
		t.Errorf("PathFor = %q", got)
	}
	got := ArtifactDir("/repo/.agentum/worktrees/abc-123", "abc-123", "spec")
	want := filepath.Join("/repo/.agentum/worktrees/abc-123", ".agentum", "abc-123", ".ag-artifacts", "spec")
	if got != want {
		t.Errorf("ArtifactDir = %q, want %q", got, want)
	}
}

func TestManager_Create_Idempotent_Remove(t *testing.T) {
	// Not parallel: each subtest builds on the same repo state, and Create/Remove
	// on the same repo must be observed in order.
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}

	manager := New()
	taskID := "task-001"

	// First create: makes the worktree. Empty baseCommit → HEAD (the
	// pre-F.6.1 path; production always passes a resolved SHA).
	worktree, err := manager.Create(t.Context(), repo, taskID, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if worktree.Branch != "agentum/task-001" {
		t.Errorf("Branch = %q", worktree.Branch)
	}
	if !isWorktree(context.Background(), worktree.Root) {
		t.Fatalf("worktree not created at %s", worktree.Root)
	}
	// The branch is checked out in the worktree.
	assertBranchCheckedOut(t, worktree.Root, "agentum/task-001")

	// Second create: idempotent — returns the existing worktree, no error.
	secondWorktree, err := manager.Create(t.Context(), repo, taskID, "")
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if secondWorktree.Root != worktree.Root {
		t.Error("idempotent Create returned a different path")
	}

	// RemoveWorktree: tears down the working tree only. The branch must survive
	// (F.6.1 AC #3 — branch + commits remain resolvable after teardown).
	if err := manager.RemoveWorktree(t.Context(), repo, taskID); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if isWorktree(context.Background(), worktree.Root) {
		t.Error("worktree still present after RemoveWorktree")
	}
	assertBranchListHas(t, repo, "agentum/task-001", true)

	// DeleteBranch: explicit cleanup removes the branch now. Idempotent.
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	assertBranchListHas(t, repo, "agentum/task-001", false)

	// Both ops idempotent on a fully-cleaned task: no-op, no error.
	if err := manager.RemoveWorktree(t.Context(), repo, taskID); err != nil {
		t.Errorf("second RemoveWorktree should be a no-op, got: %v", err)
	}
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Errorf("second DeleteBranch should be a no-op, got: %v", err)
	}
}

// TestWorktree_MoveBreaksLinkAndRepairRestores pins the recovery path for a
// repository that moved on disk: git writes absolute paths into both halves of
// the worktree linkage, so after the move the worktree directory is fully
// present while git refuses to enter it — presence alone must not read as
// "healthy worktree" (that was the old isWorktree), and Repair is what turns
// the stale links live again, after which Create returns the SAME worktree
// rather than trying to rebuild the lineage.
func TestWorktree_MoveBreaksLinkAndRepairRestores(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()
	const taskID = "task-moved"
	worktree, err := manager.Create(t.Context(), repo, taskID, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	headBeforeMove, err := manager.HeadCommit(t.Context(), worktree.Root)
	if err != nil {
		t.Fatalf("read head before move: %v", err)
	}

	movedRepo := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, movedRepo); err != nil {
		t.Fatalf("move repo: %v", err)
	}
	movedWorktreeRoot := PathFor(movedRepo, taskID)

	// The directory (and its .git file) traveled with the repository, but the
	// link is dead: presence is true, liveness is not.
	if !DirPresent(movedWorktreeRoot) {
		t.Fatal("the worktree directory must exist after the repository moved")
	}
	if isWorktree(context.Background(), movedWorktreeRoot) {
		t.Fatal("a moved worktree's stale link must not read as a live worktree")
	}

	if err := manager.Repair(t.Context(), movedRepo, taskID); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !isWorktree(context.Background(), movedWorktreeRoot) {
		t.Fatal("Repair must make the moved worktree live again")
	}
	headAfterRepair, err := manager.HeadCommit(t.Context(), movedWorktreeRoot)
	if err != nil {
		t.Fatalf("read head after repair: %v", err)
	}
	if headAfterRepair != headBeforeMove {
		t.Fatalf("head moved across repair: %q -> %q", headBeforeMove, headAfterRepair)
	}

	// Create after repair returns the SAME worktree (idempotent), not a
	// rebuild: the lineage a run left behind is the whole point of repairing
	// instead of recreating.
	restored, err := manager.Create(t.Context(), movedRepo, taskID, headBeforeMove)
	if err != nil {
		t.Fatalf("Create after repair: %v", err)
	}
	if restored.Root != movedWorktreeRoot {
		t.Fatalf("Create rebuilt the worktree at %s, want the repaired %s", restored.Root, movedWorktreeRoot)
	}

	// Repair is idempotent: a second run on a healthy worktree is a no-op.
	if err := manager.Repair(t.Context(), movedRepo, taskID); err != nil {
		t.Fatalf("idempotent Repair: %v", err)
	}
}

// TestManager_RemoveWorktreeAfterRepoMove pins the teardown path after a
// repository moved: the worktree's stale links must be repaired BEFORE
// removal, or the removal is a silent no-op — the liveness check isWorktree
// performs says "not a worktree" about a directory that is still on disk,
// together with its admin metadata. Repair, then remove, and both the
// working directory and the .git/worktrees record must be gone.
func TestManager_RemoveWorktreeAfterRepoMove(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()
	const taskID = "task-remove-after-move"
	if _, err := manager.Create(t.Context(), repo, taskID, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	movedRepo := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, movedRepo); err != nil {
		t.Fatalf("move repo: %v", err)
	}
	// The worktree traveled with the repository; its recorded links still
	// point at the old location.
	movedWorktreeRoot := PathFor(movedRepo, taskID)

	// Without repair the removal is a no-op: this is the regression guard.
	if err := manager.RemoveWorktree(t.Context(), movedRepo, taskID); err != nil {
		t.Fatalf("RemoveWorktree on stale links: %v", err)
	}
	if !DirPresent(movedWorktreeRoot) {
		t.Fatal("control check failed: the stale worktree directory vanished without repair — the no-op premise changed")
	}

	if err := manager.Repair(t.Context(), movedRepo, taskID); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if err := manager.RemoveWorktree(t.Context(), movedRepo, taskID); err != nil {
		t.Fatalf("RemoveWorktree after Repair: %v", err)
	}
	if DirPresent(movedWorktreeRoot) {
		t.Fatal("the worktree directory survived a repaired removal")
	}
	worktreeAdminDir := filepath.Join(movedRepo, ".git", "worktrees", taskID)
	if _, statErr := os.Stat(worktreeAdminDir); !os.IsNotExist(statErr) {
		t.Fatalf("the .git/worktrees record survived: stat err = %v", statErr)
	}
}

// assertBranchCheckedOut confirms the worktree's HEAD points at branch. The
// git call + parse lives here so the call site is a single assertion (no inline
// else-if that would inflate the caller's complexity).
func assertBranchCheckedOut(t *testing.T, wtRoot, branch string) {
	t.Helper()
	out := strings.TrimSpace(mustGit(t, wtRoot, "rev-parse", "--abbrev-ref", "HEAD"))
	if out != branch {
		t.Errorf("HEAD branch = %q, want %q", out, branch)
	}
}

// assertBranchListHas confirms the project repo's `git branch --list` for
// branch reports present (want true) or absent (want false).
func assertBranchListHas(t *testing.T, repo, branch string, want bool) {
	t.Helper()
	out := mustGit(t, repo, "branch", "--list", branch)
	if present := strings.TrimSpace(out) != ""; present != want {
		t.Errorf("branch %q present=%v, want %v (output=%q)", branch, present, want, out)
	}
}

func TestManager_EnsureIgnored(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()

	if _, err := manager.Create(t.Context(), repo, "task-002", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The repo's local excludes file must now ignore .agentum/.
	out, err := git(t.Context(), repo, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		t.Fatalf("locate excludes: %v", err)
	}
	excludePath := filepath.Join(repo, strings.TrimSpace(string(out)))
	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read excludes: %v", err)
	}
	if !strings.Contains(string(content), ".agentum/") {
		t.Errorf(".agentum/ not in excludes file; got:\n%s", content)
	}
	// .agentum/ must NOT show as untracked in the project repo.
	if out, err := git(t.Context(), repo, "status", "--porcelain"); err != nil {
		t.Fatalf("status: %v", err)
	} else if strings.Contains(string(out), ".agentum") {
		t.Errorf(".agentum shows as untracked; got:\n%s", out)
	}
}

// initRepoWithCommit turns dir into a git repo with one committed file. git
// worktree add requires at least one commit (it refuses an empty repo).
func initRepoWithCommit(dir string) error {
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@agentum"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &setupError{args: args, out: out, err: err}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "README"},
		{"commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &setupError{args: args, out: out, err: err}
		}
	}
	return nil
}

type setupError struct {
	args []string
	out  []byte
	err  error
}

func (setupErr *setupError) Error() string {
	return "git " + setupErr.args[0] + ": " + setupErr.err.Error() + " (" + string(setupErr.out) + ")"
}

// TestManager_FileAtCommit proves a file is read exactly as it existed at a
// commit, independently of later edits. This is the agent-immutability seam for
// the project checks registry: an agent that edits .agentum.yaml inside a
// worktree cannot change what FileAtCommit returns for the lineage anchor.
func TestManager_FileAtCommit(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Commit a registry file, capturing that commit's SHA.
	original := []byte("api: agentum/v1\nchecks: []\n")
	if err := os.WriteFile(filepath.Join(repo, ".agentum.yaml"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "add", ".agentum.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "commit", "--quiet", "-m", "add registry"); err != nil {
		t.Fatal(err)
	}
	anchor, err := headOf(repo)
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	// Now modify the file on top of the anchor and commit — simulate an agent
	// weakening the registry.
	if err := os.WriteFile(filepath.Join(repo, ".agentum.yaml"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "add", ".agentum.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "commit", "--quiet", "-m", "weaken"); err != nil {
		t.Fatal(err)
	}

	manager := New()
	got, err := manager.FileAtCommit(t.Context(), repo, anchor, ".agentum.yaml")
	if err != nil {
		t.Fatalf("FileAtCommit: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("FileAtCommit must return the anchor's content, not a later edit; got %q", got)
	}

	// A path absent at the commit is reported as os.ErrNotExist, not a hard
	// failure. errors.Is traverses the %w wrap; os.IsNotExist is not reliable
	// across wrapped errors.
	if _, err := manager.FileAtCommit(t.Context(), repo, anchor, "never-existed.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist for absent path, got %v", err)
	}
}

func gitInRepo(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		return &setupError{args: args, out: out, err: err}
	}
	return nil
}

func headOf(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TestManager_Commit_DirtyTreeCreatesCommitAndLeavesClean is the E1 foundation
// test: the orchestrator's Commit must turn a dirty working tree into a real
// commit on the task branch, authored by the orchestrator, leaving the tree
// clean. Without this, every post-stage checkpoint is the base SHA and the
// agent's uncommitted work is silently discarded at teardown — the defect class
// this whole PR exists to close.
func TestManager_Commit_DirtyTreeCreatesCommitAndLeavesClean(t *testing.T) {
	t.Parallel()
	repo, wt := setupWorktreeWithIdentity(t)

	// Simulate an agent writing files without committing — the normal state,
	// since no agent role carries git.delivery.
	mustWrite(wt.Root, "feature.txt", "new work")

	commitSHA, created, err := New().Commit(t.Context(), wt.Root, "agentum: checkpoint after stage spec")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !created {
		t.Fatal("Commit returned created=false on a dirty tree; the agent's work was not committed")
	}

	// The tree is now clean — the commit captured everything.
	clean, err := New().IsClean(t.Context(), wt.Root)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Error("worktree not clean after Commit; the checkpoint did not capture the working state")
	}

	// The commit is a real commit beyond the base, authored by the orchestrator
	// (not a human), on the task branch, and it contains the agent's work.
	assertCommitBeyondBase(t, repo, commitSHA)
	assertCommitAuthor(t, repo, commitSHA, orchestratorIdentityName, orchestratorIdentityEmail)
	assertCommitContainsFile(t, repo, commitSHA, "feature.txt", "new work")
	assertBranchAtCommit(t, repo, "agentum/task-commit-dirty", commitSHA)
}

// TestManager_Commit_CleanTreeCreatesNothing pins the no-empty-commit contract:
// a stage that produced no change records the unchanged HEAD honestly, rather
// than inserting a no-op commit into the lineage. An empty commit per stage
// would corrupt the lineage a reviewer reads — a flat line of identical
// checkpoints obscuring where real work landed.
func TestManager_Commit_CleanTreeCreatesNothing(t *testing.T) {
	t.Parallel()
	_, wt := setupWorktreeWithIdentity(t)
	baseCommit, err := New().HeadCommit(t.Context(), wt.Root)
	if err != nil {
		t.Fatalf("base HEAD: %v", err)
	}

	commitSHA, created, err := New().Commit(t.Context(), wt.Root, "agentum: checkpoint after stage spec")
	if err != nil {
		t.Fatalf("Commit on clean tree: %v", err)
	}
	if created {
		t.Error("Commit returned created=true on a clean tree; an empty commit would pollute the lineage")
	}
	if commitSHA != baseCommit {
		t.Errorf("Commit on clean tree returned %q, want the unchanged HEAD %q", commitSHA, baseCommit)
	}
}

// TestManager_Commit_SucceedsWithoutGitIdentity covers the failure mode that
// would otherwise only surface in CI or on a fresh operator server: a repo with
// no user.name / user.email configured. git commit refuses without an identity,
// so Commit passes the orchestrator's identity inline via `git -c`. This is the
// test that would have caught the defect in the environment where it bites.
func TestManager_Commit_SucceedsWithoutGitIdentity(t *testing.T) {
	t.Parallel()
	repo, wt := setupWorktreeWithoutIdentity(t)

	mustWrite(wt.Root, "feature.txt", "work")
	commitSHA, created, err := New().Commit(t.Context(), wt.Root, "agentum: checkpoint after stage spec")
	if err != nil {
		t.Fatalf("Commit without repo identity must succeed by passing identity inline: %v", err)
	}
	if !created {
		t.Fatal("Commit returned created=false on a dirty tree")
	}
	assertCommitAuthor(t, repo, commitSHA, orchestratorIdentityName, orchestratorIdentityEmail)
}

// TestManager_Commit_DoesNotSweepAgentumDir guards PR C's containment work: the
// .agentum/ artifact tree is gitignored, so artifact-dir churn (result.json,
// per-stage outputs) never enters a checkpoint commit. A checkpoint that swept
// in .agentum/ would mix orchestrator bookkeeping into the work lineage.
func TestManager_Commit_DoesNotSweepAgentumDir(t *testing.T) {
	t.Parallel()
	repo, wt := setupWorktreeWithIdentity(t)

	// Write real work AND orchestrator bookkeeping under .agentum/.
	mustWrite(wt.Root, "feature.txt", "real work")
	agentumArtifactDir := filepath.Join(wt.Root, ".agentum", "task-x", ".ag-artifacts", "spec")
	if err := os.MkdirAll(agentumArtifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	mustWrite(agentumArtifactDir, "result.json", `{"status":"complete"}`)

	commitSHA, _, err := New().Commit(t.Context(), wt.Root, "agentum: checkpoint after stage spec")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// feature.txt is in the commit; .agentum/ is not.
	assertCommitContainsFile(t, repo, commitSHA, "feature.txt", "real work")
	if files := committedFiles(t, repo, commitSHA); strings.Contains(strings.Join(files, ","), ".agentum") {
		t.Errorf(".agentum/ swept into checkpoint commit: %v", files)
	}
}

// setupWorktreeWithIdentity builds a repo (with git identity configured, as the
// existing init helper does) and a worktree branched from its HEAD, mirroring
// what the runner creates per task. Returns both so a test can write into the
// worktree and assert against the repo.
func setupWorktreeWithIdentity(t *testing.T) (repo string, wt *Worktree) {
	t.Helper()
	repo = t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	manager := New()
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	wt, err = manager.Create(t.Context(), repo, "task-commit-dirty", baseCommit)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	return repo, wt
}

// setupWorktreeWithoutIdentity is setupWorktreeWithIdentity for a repo that has
// NO user.name / user.email configured — the CI / fresh-server state. Used to
// prove Commit supplies its own identity rather than depending on ambient config.
func setupWorktreeWithoutIdentity(t *testing.T) (repo string, wt *Worktree) {
	t.Helper()
	repo = t.TempDir()
	if err := initRepoWithoutIdentity(repo); err != nil {
		t.Fatalf("setup repo without identity: %v", err)
	}
	manager := New()
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	wt, err = manager.Create(t.Context(), repo, "task-commit-noidentity", baseCommit)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	return repo, wt
}

// initRepoWithoutIdentity inits a repo and commits the seed file WITHOUT setting
// user.name / user.email locally — the state that breaks a naive `git commit`.
// The seed commit itself needs an identity, so it is made with inline `-c`
// (mirroring how Commit works); subsequent commits in the worktree have none.
func initRepoWithoutIdentity(dir string) error {
	if out, err := exec.Command("git", "-C", dir, "init", "--quiet").CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello"), 0o644); err != nil {
		return err
	}
	// Seed commit with an inline identity so the repo has a HEAD to branch from;
	// the repo itself remains without a configured identity.
	for _, args := range [][]string{
		{"add", "README"},
		{"-c", "user.name=seed", "-c", "user.email=seed@agentum", "commit", "--quiet", "-m", "init"},
	} {
		fullArgs := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", fullArgs...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &setupError{args: args, out: out, err: err}
		}
	}
	return nil
}

// assertCommitBeyondBase confirms commitSHA is a real descendant of the repo's
// initial commit (not the base SHA itself), proving Commit captured work rather
// than recording a no-op.
func assertCommitBeyondBase(t *testing.T, repo, commitSHA string) {
	t.Helper()
	// A commit with a parent is a real commit; the seed commit has none.
	parents := strings.TrimSpace(mustGit(t, repo, "rev-list", "--parents", "-n", "1", commitSHA))
	fields := strings.Fields(parents)
	if len(fields) < 2 {
		t.Fatalf("checkpoint commit %s has no parent; it is not a real commit beyond the base", commitSHA)
	}
}

// assertCommitAuthor checks the commit's author identity matches the
// orchestrator's, so the audit trail distinguishes orchestrator checkpoints
// from any agent git.write commits.
func assertCommitAuthor(t *testing.T, repo, commitSHA, wantName, wantEmail string) {
	t.Helper()
	line := strings.TrimSpace(mustGit(t, repo, "show", "-s", "--format=%an <%ae>", commitSHA))
	want := wantName + " <" + wantEmail + ">"
	if line != want {
		t.Errorf("commit author = %q, want %q (the orchestrator must author checkpoints)", line, want)
	}
}

// assertCommitContainsFile asserts the file's content at commitSHA matches body,
// proving the checkpoint captured the working state rather than an empty tree.
func assertCommitContainsFile(t *testing.T, repo, commitSHA, path, body string) {
	t.Helper()
	got, err := New().FileAtCommit(t.Context(), repo, commitSHA, path)
	if err != nil {
		t.Fatalf("read %s at %s: %v", path, commitSHA, err)
	}
	if string(got) != body {
		t.Errorf("content of %s at checkpoint = %q, want %q", path, got, body)
	}
}

// committedFiles lists the file paths a commit touched, for the .agentum/
// containment check.
func committedFiles(t *testing.T, repo, commitSHA string) []string {
	t.Helper()
	out := strings.TrimSpace(mustGit(t, repo, "show", "--stat", "--name-only", "--format=", commitSHA))
	return strings.Split(out, "\n")
}

// assertBranchAtCommit confirms the named branch ref resolves to commitSHA,
// proving Commit landed on the branch rather than a detached HEAD.
func assertBranchAtCommit(t *testing.T, repo, branch, commitSHA string) {
	t.Helper()
	tip := strings.TrimSpace(mustGit(t, repo, "rev-parse", branch))
	if tip != commitSHA {
		t.Errorf("branch %s tip = %s, want the checkpoint commit %s (Commit must land on the task branch)", branch, tip, commitSHA)
	}
}
