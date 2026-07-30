package worktree

import (
	"errors"
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
	if !isWorktree(worktree.Root) {
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
	if isWorktree(worktree.Root) {
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
